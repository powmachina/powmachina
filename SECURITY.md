# Reporting a security problem

**Do not open an issue or a pull request for a security bug.** A public description of a hole is
an attack manual until the networks have it fixed.

Write to **security@powmachina.com**. Encrypt it if you like. Tell us what you found, how to
reproduce it and what you think the impact is; a proof of concept helps but is not required.

You will get an acknowledgement within 72 hours. We will tell you what we think the severity is
and when a fix is likely, and we will say when it is safe to publish. If you want credit in the
release notes, say so — anonymity is equally fine.

## What counts

Consensus splits, remote crashes, anything that lets a node be fed a false chain, theft or loss of
funds, key material leaking out of the wallet, and anything that lets one peer deny service to the
network at low cost.

Known weak spots are written down rather than hidden — they are in the security notes at
[powmachina.com](https://powmachina.com).

## What does not

Reports from an automated scanner with no demonstrated impact, findings that need an attacker to
already have the machine or the wallet password, and missing hardening that costs more than it
defends.

## Scope

This repository: the node, the miner, the wallet, the CUDA kernel and the release tooling. The
website and the pool are run separately; a problem there goes to the same address.
