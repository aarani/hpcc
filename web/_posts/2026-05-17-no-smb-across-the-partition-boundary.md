---
title: "No SMB across the partition boundary"
date: 2026-05-17
description: >-
  Why hpcc's Windows path runs an agent over HvSocket instead of taking
  Microsoft's default VSMB share into a Hyper-V isolated container.
---

The Windows runtime landed last week. It's the second isolation backend in the tree, sitting behind the same `Runtime` interface as the Linux/Firecracker side: containerd + runhcs, Hyper-V isolation for the security story, process isolation for CI hosts without nested virt. What's worth a post is the one decision in it that took the most time to defend — and that I expect to be asked about repeatedly.

## The obvious path is VSMB

If you want to share a host directory into a Hyper-V isolated container, the documented Microsoft path is **VSMB** — Virtual SMB. The host runs an SMB server, the utility VM mounts it across the partition boundary, your container sees a normal filesystem. It's what every Windows-container tutorial reaches for. It works.

It's also the same protocol family that absorbed EternalBlue, and that has produced a steady drip of kernel-mode RCE CVEs for two decades. The reason it keeps producing them isn't an indictment of the SMB maintainers — it's that SMB is an enormous parser surface (auth, dialect negotiation, oplocks, leases, named pipes, DFS) running inside a kernel that has to honor wire-format compatibility back to the 1990s.

hpcc's entire pitch is that the kernel+VM boundary is a line a security review actually recognises. Stapling an SMB parser across that line gives the threat model back. The auditor question stops being *"is this isolated?"* and becomes *"what's your patch cadence for the SMB stack inside the utility VM?"* — which is not a question anyone wants to answer in a regulated environment.

## What the Linux side does, mirrored

On Linux, the host↔guest channel is one vsock device carrying a single bidirectional gRPC stream — `AgentService.Exec`. Header + input file chunks in, stdio + result + output file chunks back. The wire protocol is one we own. There is no filesystem.

The Windows port is the same shape:

- `hpcc-agent.exe` is staged into the container at `C:\.hpcc` and set as PID 1.
- The agent listens on **HvSocket** — Hyper-V's analogue of vsock, a socket family that crosses the partition boundary without involving any filesystem protocol.
- After `Task.Start`, the worker resolves the utility VM's GUID from `hcsshim.GetContainers` and dials the agent over HvSocket.
- The same `AgentService.Exec` bidi stream carries the compile.

No SMB anywhere. No share. No mount across the partition boundary. The attack surface on the boundary is the gRPC parser plus our protobuf schema — code we wrote, on a wire format we control, with one function that handles one request type.

## The boring parts that took the most time

The interesting decision was easy. The work was elsewhere:

- **Path normalization.** Windows hands you paths with `\\?\` prefixes for long-path support, UNC paths for network locations, mixed casing from case-insensitive filesystems, and short (8.3) names from legacy APIs. We strip `\\?\`, reject UNC up front (no remote sources, ever), and case-fold inside the cache digest so a checkout under `C:\Users\Afshin\src\…` and one under `C:\users\afshin\src\…` hash identically.
- **Reproducibility flags for MSVC.** GCC has `-ffile-prefix-map=/src=.` and `-Werror=date-time`. MSVC has `/d1trimfile:/src` and `/PDBSourcePath:/src`. Both families are auto-injected per-detected-compiler so embedded paths in `.obj` and `.pdb` files don't pin a job's hash to its staging directory.
- **Process isolation as a peer, not a fallback.** CI runners without nested virt still need to run the tests. Process isolation keeps `pause` + `Task.Exec` + `copyTree` behind the same `Runtime` interface — same Exec stream shape, weaker boundary, honestly labeled in the audit row.

## What's next

The Windows side is feature-complete behind the `Runtime` interface, but there are two open items on the phase 4 list that touch it:

- **VM-crash reaping with scheduler reroute.** The runtime surfaces process exit cleanly; the worker doesn't yet notify the scheduler to drop a dead VM so a follow-on dial doesn't race.
- **Toolchain parity check.** An operator currently pins the image's compiler patch version against the host by hand. An automatic probe — host gcc/cl version reported back to the scheduler at session start, compared against the image's declared version — is the last cross-developer-hit-rate hazard on Windows.

Neither is a security item. The boundary story is done.
