//! Shell of the desktop wallet.
//!
//! It does three things and nothing more: starts the node and `powmachina-wallet-rpc` as
//! sidecars, mints a single-run token for each, and tells the UI where to ask. **No wallet
//! logic lives here** — keys, signatures and block scanning are in Go (see `gui/README.md`).
//!
//! Tokens are generated fresh on every start and never stored. The sidecars listen on
//! 127.0.0.1 on a port the system hands out; the app learns it from their first line of
//! output, so two running copies never fight over the same port.

use std::sync::Mutex;

/// HTTP line separator. A constant so that it cannot be written "almost right".
const CRLF: &str = "\r\n";

use rand::RngCore;
use serde::Serialize;
use tauri::{Emitter, Manager, State};
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

/// Connection details for a sidecar.
#[derive(Clone, Serialize)]
pub struct Rpc {
    /// Full base address, e.g. `http://127.0.0.1:41234`.
    pub url: String,
    /// Token valid for this run of the app only.
    pub token: String,
    /// `testnet` / `mainnet` / `regtest` — the UI labels the network from this.
    pub network: String,
}

#[derive(Default)]
struct Backend {
    rpc: Mutex<Option<Rpc>>,
    /// Children the app started: the node and the wallet. Both must die with the window —
    /// a sidecar that outlives the app keeps the data directory and wallet file locked.
    children: Mutex<Vec<CommandChild>>,
    /// RPC address of the node (our own or a given one) — the wallet asks it for blocks.
    node: Mutex<Option<String>>,
}

/// The UI asks for connection state. **No token is returned** — it stays in Rust and never
/// reaches the page; calls go through `rpc_call`.
#[derive(Clone, Serialize)]
pub struct RpcInfo {
    pub ready: bool,
    pub network: String,
}

#[tauri::command]
fn rpc_info(state: State<'_, Backend>) -> RpcInfo {
    let g = state.rpc.lock().ok().and_then(|g| g.clone());
    RpcInfo {
        ready: g.is_some(),
        network: g.map(|r| r.network).unwrap_or_default(),
    }
}

/// rpc_call sends JSON-RPC to a sidecar and returns the reply.
///
/// Why through Rust instead of `fetch` from the page: the webview runs on the `tauri://`
/// origin, so a call to `http://127.0.0.1:<port>` is cross-origin and the browser drops it
/// without CORS headers ("Load failed"). Adding CORS to wallet-rpc would open it to **any**
/// page in the browser that guesses the port. This way neither token nor address leaves Rust.
#[tauri::command]
async fn rpc_call(
    state: State<'_, Backend>,
    method: String,
    params: serde_json::Value,
    wallet: Option<String>,
) -> Result<serde_json::Value, String> {
    let rpc = state
        .rpc
        .lock()
        .ok()
        .and_then(|g| g.clone())
        .ok_or_else(|| "the wallet is not running yet".to_string())?;

    let path = match wallet.as_deref() {
        Some(w) if !w.is_empty() => format!("/wallet/{w}"),
        _ => "/".to_string(),
    };
    let body = serde_json::json!({"jsonrpc":"1.0","id":"ui","method":method,"params":params});
    let out = tauri::async_runtime::spawn_blocking(move || {
        post_json(&rpc.url, &path, &rpc.token, &body.to_string())
    })
    .await
    .map_err(|e| e.to_string())??;

    let v: serde_json::Value =
        serde_json::from_str(&quote_decimals(&out)).map_err(|e| e.to_string())?;
    if let Some(err) = v.get("error") {
        if !err.is_null() {
            let msg = err
                .get("message")
                .and_then(|m| m.as_str())
                .unwrap_or("unknown error");
            return Err(msg.to_string());
        }
    }
    Ok(v.get("result").cloned().unwrap_or(serde_json::Value::Null))
}

/// quote_decimals turns every JSON number with a fraction or an exponent into a string holding
/// the same characters; whole numbers stay numbers.
///
/// The wallet writes amounts as numbers with eight decimals (`0.00000001`), the way bitcoind
/// does. Parsed as a number that becomes a float — here and again in the page — and the page
/// then sees `1e-8`, or a sum that is off in the last place. As text it reaches the page
/// exactly as the wallet wrote it, and the page counts in whole cogs from there.
fn quote_decimals(json: &str) -> String {
    let src = json.as_bytes();
    let mut out = Vec::with_capacity(src.len() + 64);
    let mut i = 0;
    while i < src.len() {
        let c = src[i];
        if c == b'"' {
            // a string is copied as it is, escapes included
            let start = i;
            i += 1;
            while i < src.len() && src[i] != b'"' {
                i += if src[i] == b'\\' { 2 } else { 1 };
            }
            i = (i + 1).min(src.len());
            out.extend_from_slice(&src[start..i]);
        } else if c == b'-' || c.is_ascii_digit() {
            let start = i;
            while i < src.len() && matches!(src[i], b'0'..=b'9' | b'-' | b'+' | b'.' | b'e' | b'E')
            {
                i += 1;
            }
            let token = &src[start..i];
            if token.iter().any(|b| matches!(b, b'.' | b'e' | b'E')) {
                out.push(b'"');
                out.extend_from_slice(token);
                out.push(b'"');
            } else {
                out.extend_from_slice(token);
            }
        } else {
            out.push(c);
            i += 1;
        }
    }
    // Only ASCII quotes were added, and only between ASCII characters.
    String::from_utf8(out).unwrap_or_else(|_| json.to_string())
}

/// post_json sends a POST to a local HTTP server and returns the response body.
///
/// Hand-written, with no HTTP crate: this is a connection to `127.0.0.1`, with a body whose
/// length we know, answered by **our own** server. Pulling in reqwest and a whole TLS stack
/// for that would cost more than forty lines that do exactly this and nothing else.
fn post_json(url: &str, path: &str, token: &str, body: &str) -> Result<String, String> {
    use std::io::{Read, Write};
    let addr = url.trim_start_matches("http://");
    let mut sock = std::net::TcpStream::connect(addr).map_err(|e| e.to_string())?;
    sock.set_read_timeout(Some(std::time::Duration::from_secs(120)))
        .map_err(|e| e.to_string())?;

    // Headers are built line by line and joined with CRLF on purpose: a multi-line string
    // with continuations is exactly where indentation gets preserved by accident one day —
    // and a header starting with a space is an invalid request to the server, not a typo.
    let head = [
        format!("POST {path} HTTP/1.1"),
        format!("Host: {addr}"),
        format!("Authorization: Bearer {token}"),
        "Content-Type: application/json".to_string(),
        format!("Content-Length: {}", body.len()),
        "Connection: close".to_string(),
    ]
    .join(CRLF);
    let req = format!("{head}{CRLF}{CRLF}{body}");

    sock.write_all(req.as_bytes()).map_err(|e| e.to_string())?;
    let mut raw = Vec::new();
    sock.read_to_end(&mut raw).map_err(|e| e.to_string())?;
    http_body(&raw)
}

/// http_body takes a raw HTTP/1.1 response apart and returns its body.
///
/// The server decides how the body is framed, not us: Go sends a short reply with
/// `Content-Length` and a longer one (a few kilobytes — a list of twenty payments) as
/// `Transfer-Encoding: chunked`, even on a connection it is about to close. Taking
/// "everything after the blank line" works for the first kind only; with the second the JSON
/// arrives wrapped in chunk sizes and does not parse. A wallet reading it that way works on
/// its first day and stops showing anything once it has enough history.
fn http_body(raw: &[u8]) -> Result<String, String> {
    let cut = raw.windows(4).position(|w| w == b"\r\n\r\n").ok_or_else(|| {
        format!(
            "response with no header ({} B): {}",
            raw.len(),
            String::from_utf8_lossy(raw).chars().take(160).collect::<String>()
        )
    })?;
    let head = String::from_utf8_lossy(&raw[..cut]);
    let body = &raw[cut + 4..];
    let mut lines = head.lines();
    // The status line matters only when we are refused — a JSON-RPC error is the caller's.
    let status = lines.next().unwrap_or_default();
    if status.contains(" 401") || status.contains(" 403") {
        return Err("the wallet rejected the token".into());
    }
    let mut chunked = false;
    let mut length: Option<usize> = None;
    for line in lines {
        let Some((name, value)) = line.split_once(':') else {
            continue;
        };
        let value = value.trim();
        if name.eq_ignore_ascii_case("transfer-encoding") {
            chunked = value.to_ascii_lowercase().contains("chunked");
        } else if name.eq_ignore_ascii_case("content-length") {
            length = value.parse().ok();
        }
    }
    let bytes = if chunked {
        dechunk(body)?
    } else {
        // A reply shorter than it says it is was cut off on the way. Half a JSON document
        // must not be handed on as if it were the answer.
        match length {
            Some(n) if body.len() < n => return Err(cut_short()),
            Some(n) => body[..n].to_vec(),
            None => body.to_vec(),
        }
    };
    String::from_utf8(bytes).map_err(|_| "the reply is not valid UTF-8".to_string())
}

fn cut_short() -> String {
    "the reply was cut short".to_string()
}

/// dechunk joins the pieces of a chunked body: `<size in hex>[;extension]`, CRLF, that many
/// bytes, CRLF — until a chunk of size zero.
fn dechunk(mut rest: &[u8]) -> Result<Vec<u8>, String> {
    let mut out = Vec::with_capacity(rest.len());
    loop {
        let eol = rest
            .windows(2)
            .position(|w| w == b"\r\n")
            .ok_or_else(cut_short)?;
        let line = std::str::from_utf8(&rest[..eol]).map_err(|_| cut_short())?;
        let hex = line.split(';').next().unwrap_or_default().trim();
        let size =
            usize::from_str_radix(hex, 16).map_err(|_| format!("bad chunk size {hex:?}"))?;
        rest = &rest[eol + 2..];
        if size == 0 {
            return Ok(out);
        }
        if rest.len() < size || rest.len() - size < 2 {
            return Err(cut_short());
        }
        out.extend_from_slice(&rest[..size]);
        rest = &rest[size + 2..];
    }
}

/// The network is chosen at startup: `POWM_NETWORK=testnet|mainnet|regtest`.
/// Mainnet has no genesis yet, so testnet is the default.
fn network() -> String {
    match std::env::var("POWM_NETWORK").unwrap_or_default().as_str() {
        "mainnet" => "mainnet".into(),
        "regtest" => "regtest".into(),
        _ => "testnet".into(),
    }
}

fn new_token() -> String {
    let mut b = [0u8; 24];
    rand::thread_rng().fill_bytes(&mut b);
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// Where wallet data lives. People pick their own wallet files, but the default location
/// has to be one and the same on every platform — otherwise they lose it on reinstall.
fn data_dir(app: &tauri::AppHandle) -> std::path::PathBuf {
    app.path()
        .app_data_dir()
        .unwrap_or_else(|_| std::path::PathBuf::from("."))
}

#[tauri::command]
fn default_wallet_dir(app: tauri::AppHandle) -> String {
    let d = data_dir(&app).join("wallets");
    let _ = std::fs::create_dir_all(&d);
    d.to_string_lossy().into_owned()
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_opener::init())
        .manage(Backend::default())
        .invoke_handler(tauri::generate_handler![rpc_info, rpc_call, default_wallet_dir])
        .setup(|app| {
            let net = network();
            let handle = app.handle().clone();
            let data = data_dir(&handle);
            std::fs::create_dir_all(data.join("wallets")).ok();

            // 1) The node. Either we start one, or `POWM_NODE` points us at an existing one.
            //
            // Our own node is the default on purpose: a wallet that trusts someone else's
            // server also trusts it about **what it received** — it does not verify proof of
            // work on the blocks it is handed. With our own node that question disappears.
            let node_token = new_token();
            let node_addr = match std::env::var("POWM_NODE") {
                Ok(a) if !a.is_empty() => {
                    eprintln!("[gui] using the node given in POWM_NODE: {a}");
                    Some(a)
                }
                _ => {
                    spawn_node(&handle, &net, &node_token, &data)?;
                    None // we learn the address from its output
                }
            };
            if let Some(a) = node_addr.clone() {
                handle.state::<Backend>().node.lock().unwrap().replace(a);
            }

            // 2) The wallet. When we start the node ourselves it has to wait for the node's
            //    address — otherwise it would ask a port that does not exist yet.
            let wtoken = new_token();
            if node_addr.is_some() {
                spawn_wallet(&handle, &net, &wtoken, &node_token)?;
            }
            Ok(())
        })
        .on_window_event(|window, event| {
            // The sidecars hold the data directory and the wallet file open. If the app
            // vanished without stopping them, they would linger and keep the locks.
            if let tauri::WindowEvent::Destroyed = event {
                if let Some(state) = window.try_state::<Backend>() {
                    for child in state.children.lock().unwrap().drain(..) {
                        let _ = child.kill();
                    }
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("the app failed to start");
}

/// spawn_node starts our own node. It runs **without `-listen`** (outbound connections
/// only), so it asks for no firewall hole and Windows stays quiet.
///
/// `-dag=sync` is a trade-off worth knowing: while catching up the node builds the full DAG
/// (~1 GiB, 0.62 ms per header) and drops to light at the tip (26 MiB, 63 ms), where it checks a
/// sample of the shares a block settles. `POWM_DAG=light` stays frugal the whole time — good on an
/// old laptop, at the price of a slower first sync; `POWM_DAG=full` verifies every share (~3.5 GiB).
fn spawn_node(
    handle: &tauri::AppHandle,
    net: &str,
    token: &str,
    data: &std::path::Path,
) -> Result<(), Box<dyn std::error::Error>> {
    let dag = match std::env::var("POWM_DAG").unwrap_or_default().as_str() {
        "light" => "light",
        "full" => "full",
        _ => "sync",
    };
    let (mut rx, child) = handle
        .shell()
        .sidecar("powmachinad")?
        .env("POWM_RPC_TOKEN", token.to_string())
        .args([
            format!("--{net}"),
            "--datadir".into(),
            data.join("node").to_string_lossy().into_owned(),
            "--rpc".into(),
            "127.0.0.1:0".into(),
            format!("--dag={dag}"),
        ])
        .spawn()?;
    handle.state::<Backend>().children.lock().unwrap().push(child);

    let h = handle.clone();
    let net = net.to_string();
    let node_token = token.to_string();
    tauri::async_runtime::spawn(async move {
        while let Some(ev) = rx.recv().await {
            let line = match &ev {
                CommandEvent::Stdout(b) | CommandEvent::Stderr(b) => {
                    String::from_utf8_lossy(b).to_string()
                }
                CommandEvent::Terminated(t) => {
                    let _ = h.emit("node-exited", format!("{t:?}"));
                    continue;
                }
                _ => continue,
            };
            eprint!("[node] {line}");
            // The node reports its RPC address only once the system assigns it. Starting
            // the wallet earlier would point it at a port that does not exist.
            if line.contains("RPC listening on ") {
                if let Some(addr) = listen_addr(&line) {
                    eprintln!("[gui] node listening on {addr}");
                    if let Some(state) = h.try_state::<Backend>() {
                        let first = state.node.lock().unwrap().replace(addr.clone()).is_none();
                        if first {
                            let wtoken = new_token();
                            if let Err(e) = spawn_wallet(&h, &net, &wtoken, &node_token) {
                                eprintln!("[gui] the wallet failed to start: {e}");
                            }
                        }
                    }
                }
            }
        }
    });
    Ok(())
}

/// spawn_wallet starts wallet-rpc against the node address we already know.
fn spawn_wallet(
    handle: &tauri::AppHandle,
    net: &str,
    token: &str,
    node_token: &str,
) -> Result<(), Box<dyn std::error::Error>> {
    let node_addr = handle
        .state::<Backend>()
        .node
        .lock()
        .unwrap()
        .clone()
        .unwrap_or_default();

    let mut args = vec![
        format!("--{net}"),
        "--allow-manage".into(),
        "--rpc-bind".into(),
        // Port 0 = let the system pick. Two running copies then never collide and we do
        // not have to guess what is free.
        "127.0.0.1:0".into(),
    ];
    if !node_addr.is_empty() {
        args.push("--daemon-address".into());
        args.push(node_addr);
    }

    let (mut rx, child) = handle
        .shell()
        .sidecar("powmachina-wallet-rpc")?
        .env("POWM_WALLET_RPC_TOKEN", token.to_string())
        // Node token: the wallet uses it to ask for blocks. With our own node it is our
        // token; with someone else's node the user supplies it via POWM_RPC_TOKEN.
        .env(
            "POWM_RPC_TOKEN",
            std::env::var("POWM_RPC_TOKEN").unwrap_or_else(|_| node_token.to_string()),
        )
        .args(args)
        .spawn()?;
    handle.state::<Backend>().children.lock().unwrap().push(child);

    let h = handle.clone();
    let net = net.to_string();
    let token = token.to_string();
    tauri::async_runtime::spawn(async move {
        while let Some(ev) = rx.recv().await {
            let line = match &ev {
                CommandEvent::Stdout(b) | CommandEvent::Stderr(b) => {
                    String::from_utf8_lossy(b).to_string()
                }
                CommandEvent::Terminated(t) => {
                    let _ = h.emit("wallet-rpc-exited", format!("{t:?}"));
                    continue;
                }
                _ => continue,
            };
            eprint!("[wallet-rpc] {line}");
            if line.contains("wallet-rpc") && line.contains("listening on ") {
                if let Some(addr) = listen_addr(&line) {
                    let rpc = Rpc {
                        url: format!("http://{addr}"),
                        token: token.clone(),
                        network: net.clone(),
                    };
                    if let Some(state) = h.try_state::<Backend>() {
                        state.rpc.lock().unwrap().replace(rpc.clone());
                    }
                    eprintln!("[gui] wallet listening on http://{addr}");
                    let _ = h.emit(
                        "wallet-rpc-ready",
                        RpcInfo { ready: true, network: rpc.network.clone() },
                    );
                }
            }
        }
    });
    Ok(())
}

/// listen_addr pulls `host:port` out of the line a sidecar prints when it starts listening.
fn listen_addr(line: &str) -> Option<String> {
    let rest = line.split("listening on ").nth(1)?;
    let addr = rest.split_whitespace().next()?.trim_end_matches(',');
    if addr.contains(':') {
        Some(addr.to_string())
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::{http_body, listen_addr, quote_decimals};

    #[test]
    fn amounts_reach_the_page_as_text() {
        let got = quote_decimals(
            r#"{"result":{"amount":0.00000001,"fee":-0.00054000,"big":20999999.99999999,"height":1068,"neg":-3,"sci":1e-8},"error":null,"id":"ui"}"#,
        );
        assert_eq!(
            got,
            r#"{"result":{"amount":"0.00000001","fee":"-0.00054000","big":"20999999.99999999","height":1068,"neg":-3,"sci":"1e-8"},"error":null,"id":"ui"}"#
        );
        let v: serde_json::Value = serde_json::from_str(&got).unwrap();
        assert_eq!(v["result"]["amount"], "0.00000001");
        assert_eq!(v["result"]["height"], 1068);
    }

    #[test]
    fn text_that_looks_like_a_number_is_left_alone() {
        for s in [
            r#"{"label":"pay 1.5 to \"bob\" \\","address":"tpowm1q9.5","n":[1,2.50,3]}"#,
            r#"{"label":"é 0.1","x":true,"y":null}"#,
            r#""unterminated 1.5"#,
            "",
        ] {
            let want = s.replace("2.50", "\"2.50\"");
            assert_eq!(quote_decimals(s), want);
        }
    }

    #[test]
    fn reads_a_reply_with_content_length() {
        let raw = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 15\r\nConnection: close\r\n\r\n{\"result\":true}";
        assert_eq!(http_body(raw).as_deref(), Ok("{\"result\":true}"));
    }

    #[test]
    fn reads_a_chunked_reply() {
        // What Go sends once the body outgrows its buffer: sizes in hex, an extension, and
        // nothing says a chunk ends on a character boundary.
        let raw = "HTTP/1.1 200 OK\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\nA\r\n{\"result\":\r\n5;x=1\r\n[1,2,\r\n6\r\n\"é\"]}\r\n0\r\n\r\n";
        assert_eq!(http_body(raw.as_bytes()).as_deref(), Ok("{\"result\":[1,2,\"é\"]}"));
        let mut split =
            b"HTTP/1.1 200 OK\r\ntransfer-encoding: Chunked\r\n\r\n2\r\n\"\xc3\r\n2\r\n\xa9\"\r\n0\r\n\r\n"
                .to_vec();
        assert_eq!(http_body(&split).as_deref(), Ok("\"é\""));
        split.truncate(split.len() - 9);
        assert!(http_body(&split).is_err(), "a chunked reply without its end is not an answer");
    }

    #[test]
    fn refuses_a_reply_that_was_cut_off() {
        assert!(http_body(b"HTTP/1.1 200 OK\r\nContent-Length: 40\r\n\r\n{\"result\":[").is_err());
        assert!(http_body(
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1F40\r\n{\"result\""
        )
        .is_err());
        assert!(http_body(
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nabc\r\n0\r\n\r\n"
        )
        .is_err());
        assert!(http_body(b"garbage").is_err());
    }

    #[test]
    fn says_when_the_token_is_refused() {
        let raw = b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n";
        assert_eq!(http_body(raw), Err("the wallet rejected the token".to_string()));
    }

    #[test]
    fn reads_address_from_log_line() {
        let line = "2026/10/04 22:11:02 wallet-rpc (testnet) listening on 127.0.0.1:41234, wallets: , walletpassphrase required: true";
        assert_eq!(listen_addr(line).as_deref(), Some("127.0.0.1:41234"));
        assert!(listen_addr("anything else").is_none());
    }
}
