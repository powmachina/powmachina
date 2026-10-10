PowMachina miner for Linux (x86-64)
===================================

Two files, kept in the same directory:

  powmachina-miner      the miner
  libpowm-cuda.so       the GPU plugin (NVIDIA)

The miner mines lane C on the CPU and lane G on every NVIDIA card it finds, both at once.
No CUDA install is needed, only an NVIDIA driver 580 or newer. Without a card, or without the
plugin next to the miner, it mines on the CPU only.

Quick start
-----------

  ./powmachina-miner
      asks what it needs (pool or solo, your address, CPU and/or GPU) and can remember the
      answers in powmachina-miner.conf next to the binary; run it with --setup to be asked
      again. The RPC token is deliberately not saved there.

  ./powmachina-miner --benchmark 60s
      measures the speed, no node and no address needed

  ./powmachina-miner --pool stratum+tcp://HOST:PORT --address powm1... --worker rig1
      mines over stratum

  ./powmachina-miner --help
      every flag

Useful flags: --gpu on|off|auto, --gpu-devices 0,2, --gpu-duty 20 (use the card 20 % of the
time), --cpu=false (GPU only), --threads N.

At every epoch the miner checks the card's hashes against the reference implementation. A card
that returns a different hash is switched off with a message — it is never left to mine blocks
the network would reject.

Large pages
-----------

Lane C is roughly a third faster with large pages. The miner tries them and says at startup
whether it got them. To grant them (2 MiB pages, ~1 per mining thread plus the dataset):

  sudo sysctl -w vm.nr_hugepages=1280

Make it survive a reboot by putting `vm.nr_hugepages=1280` in /etc/sysctl.d/. Use
--no-large-pages to skip the attempt.

Verifying the download
----------------------

The release is signed with minisign. Check it before running anything — the public key and the
command are at powmachina.com:

  minisign -Vm SHA256SUMS -P <the public key>
  sha256sum --ignore-missing -c SHA256SUMS
