# Live zvol checkpoint spike — 3 October 2026

This experimental branch extends the batch-label/host-flush work with active
volume capture for Firecracker RAM restore. The caller pauses the source VM.
`Prepare` receives the source device through
`containerd.io/snapshot/zvol/live-origin`, with its committed image parent.
Only an active volume owned by this snapshotter with that parent is accepted.

The capture flushes the host volume, snapshots it, and streams a full independent
receive into the destination. It removes temporary snapshots and commits the
private volume without consuming the running source. Subsequent children use
ordinary ZFS clones. This trades extra disk storage and a longer source pause
for independent source/checkpoint lifetimes. It does not implement zero-copy
live capture; a clone/ancestry lifecycle is a future experiment.

## Recorded timings

.20: Ryzen AI9 HX370 / NVMe; OpenZFS 2.2.2, 64 GiB sparse-file pool on host
ext4, 30 GiB ext4 guest zvols, Firecracker 1.17.0, isolated network, 2 vCPU /
1 GiB RAM. Experimental Slicer networking, polling, and mount-helper profile.
Warm, sequential SDK calls; six forks per mode/cell. Milliseconds are medians
from request to agent ready or host-side forwarded HTTP response. Checkpoint
cost is one observation, reported separately from fork latency.

| Image | Workload | Personalised agent ms | Personalised HTTP ms | Unchanged agent ms | Unchanged HTTP ms | Checkpoint seconds |
| --- | --- | --- | --- | --- | --- | --- |
| standard | counter | 152.5 | 157.3 | 146.1 | 150.8 | 1.39 |
| standard | index | 148.0 | 152.8 | 139.6 | 144.3 | 1.54 |
| min | counter | 145.7 | 150.6 | 133.9 | 138.3 | 1.50 |
| min | index | 152.7 | 157.8 | 146.2 | 151.9 | 1.59 |

The four final timer-corrected SDK rounds passed: 48 mode-comparison hot forks,
4 fresh-boot cold controls, and 4 hot forks after deletion of the source.
RAM markers/index/counter were preserved, child disk and counter changes were
independent, and in-use checkpoint deletion was refused. All VMs/commits were
removed. A failure after receiving the private volume left ZFS snapshots,
containerd snapshot metadata, and lease inventories unchanged; the source
resumed with its original RAM marker. The final pool, processes, and copied
licence were removed, and original host networking/devmapper state matched
the preflight inventory.

Guard tests use real containerd metadata to reject foreign/traversing paths,
committed/view snapshots, missing IDs, and an active snapshot with the wrong
image parent. `go test -mod=mod -race ./...` passes.

An earlier complete comparison was faster but recorded unchanged-state app
time after an extra identity exec; it was excluded. The final round measured
HTTP before that check. `--min` shows no reliable hot-fork advantage here.

Full send/receive currently holds the snapshot metadata write transaction;
other metadata operations can wait behind it. The controlled receive-failure
rollback and clean deletion were tested, not arbitrary process crashes,
transaction commit failures, full host reboots, or broad concurrency. Real
pool measurements and retained-ancestry capture need follow-up.
Personalisation does not scrub arbitrary application RAM: both modes retained
a fake cached credential and copied private userspace PRNG state. Unchanged
mode also retains guest identity/authority. These observations do not make
arbitrary live applications safe to clone.
