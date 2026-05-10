// proto carries the wire schemas shared between modules in this repo
// — currently the runner (in the main module) and the in-VM hpcc-agent
// (its own module). Lives in its own module so the agent can pull in
// the schema without dragging the rest of the main module's dependency
// graph (containerd, go-containerregistry, …) into the in-VM binary.
module github.com/aarani/hpcc/proto

go 1.26.2

require (
	google.golang.org/grpc v1.81.0
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af
)

require (
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260504160031-60b97b32f348 // indirect
)
