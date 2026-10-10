PowMachina miner for Windows (x64)
==================================

Two files, kept in the same folder:

  powmachina-miner.exe   the miner
  powm-cuda.dll          the GPU plugin (NVIDIA)

The miner mines lane C on the CPU and lane G on every NVIDIA card it finds, both at once.
No CUDA install is needed, only an NVIDIA driver 580 or newer. Without a card, or without the
DLL next to the miner, it mines on the CPU only.

Quick start
-----------

  Double-click powmachina-miner.exe
      it asks what it needs (pool or solo, your address, CPU and/or GPU) and can remember
      the answers in powmachina-miner.conf; run it with --setup to be asked again

  powmachina-miner.exe --benchmark 60s
      measures the speed, no node and no address needed

  powmachina-miner.exe --pool stratum+tcp://HOST:PORT --address powm1... --worker rig1
      mines over stratum

  powmachina-miner.exe --help
      every flag

Useful flags: --gpu on|off|auto, --gpu-devices 0,2, --gpu-duty 20 (use the card 20 % of the
time), --cpu=false (GPU only), --threads N.

At every epoch the miner checks the card's hashes against the reference implementation. A card
that returns a different hash is switched off with a message — it is never left to mine blocks
the network would reject.

If Windows refuses to start it
------------------------------

The files carry no Authenticode signature, so Windows shows "unknown publisher", and Smart App
Control, where it is on, blocks them outright. The release is signed with minisign instead:
verify SHA256SUMS against the public key published at powmachina.com before you run anything.

"powm-cuda.dll: LoadLibrary failed, error 126" means the DLL or the NVIDIA driver is missing;
"error 193" means the DLL is not a 64-bit one.
