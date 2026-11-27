package guestspec

// The contract between runwhp's Engine API and runwhp-buildkitd, BuildKit's
// daemon as runwhp runs it in a builder container: the API carries the
// docker CLI's /grpc and /session connections to it, each over a vsock
// connection of its own to BuildKitPort, opened with a BuildKitHello.

// BuildKitPort is the vsock port runwhp-buildkitd listens on in its guest.
const BuildKitPort = 1028

// What a connection to BuildKitPort carries, as its BuildKitHello says.
const (
	// BuildKitGRPC is BuildKit's gRPC API, as Docker's /grpc carries it:
	// HTTP/2 from the client's first byte.
	BuildKitGRPC = "grpc"
	// BuildKitSession is a client's session, as Docker's /session carries
	// it, which BuildKit calls back into: the hello's Headers are the
	// request's session headers.
	BuildKitSession = "session"
	// BuildKitPing asks whether the daemon is ready: it answers "ok\n".
	BuildKitPing = "ping"
)

// BuildKitHello is the first line of a connection to BuildKitPort, as
// JSON.
type BuildKitHello struct {
	Kind    string              `json:"kind"`
	Headers map[string][]string `json:"headers,omitempty"`
}

// BuilderLabel marks runwhp's builder container, the only container whose
// BuildKitPort the host connects to.
const BuilderLabel = "io.runwhp.builder"
