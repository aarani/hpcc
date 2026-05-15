package gen

// MaxCompileMessageBytes is the gRPC message-size ceiling for the
// worker.Compile RPC. gRPC's default 4 MiB is wildly insufficient for
// real compile workloads: a single Linux-kernel TU's preprocessed
// source can hit 15 MiB once <linux/*.h> finishes expanding, and the
// returned object (with debug info or kallsyms-style symbol tables)
// can be much larger again. 256 MiB is enough headroom that even the
// pathological TUs in the kernel build complete, with the request /
// response still bounded so a confused client can't OOM a worker.
//
// Both ends of the unary worker.Compile RPC apply this limit: the
// server via grpc.MaxRecvMsgSize / grpc.MaxSendMsgSize, the client
// via grpc.MaxCallRecvMsgSize / grpc.MaxCallSendMsgSize. The
// scheduler RPCs (Route, RegisterWorker, Heartbeat) stay on gRPC
// defaults — they carry routing decisions, not compile payloads.
const MaxCompileMessageBytes = 256 << 20 // 256 MiB
