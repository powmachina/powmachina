// Windows: without this, launching the app would also open a console window.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    powmachina_wallet_gui::run()
}
