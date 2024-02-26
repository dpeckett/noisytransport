# Noisy Transport

Noisy Transport is a service-to-service communication library based on the secure transport layer used by the WireGuard VPN project. Endpoints are identified by Curve25519 public keys, traffic is encrypted and authenticated using ChaCha20-Poly1305, and sent/received as UDP packets. Noisy Transport is wire compatible with WireGuard but packets aren't required to be IP datagrams.

Noisy Transport is intended to be used as a building block for higher-level protocols (eg. RPC).

## Usage

An example of how to use Noisy Transport can be found in the [examples](./examples) directory.

In order to implement higher-level protocol on top of Noisy Transport, you will need to implement a SourceSink:

```go
type SourceSink interface {
	// Read one or more packets from the Transport (without any additional headers).
	// On a successful read it returns the number of packets read, and sets
	// packet lengths within the sizes slice. len(sizes) must be >= len(bufs).
	// A nonzero offset can be used to instruct the Transport on where to begin
	// reading into each element of the bufs slice.
	Read(bufs [][]byte, sizes []int, destinations []NoisePublicKey, offset int) (int, error)

	// Write one or more packets to the transport (without any additional headers).
	// On a successful write it returns the number of packets written. A nonzero
	// offset can be used to instruct the Transport on where to begin writing from
	// each packet contained within the bufs slice.
	Write(bufs [][]byte, sources []NoisePublicKey, offset int) (int, error)

	// BatchSize returns the preferred/max number of packets that can be read or
	// written in a single read/write call. BatchSize must not change over the
	// lifetime of a Transport.
	BatchSize() int

	// Close the SourceSink.
	Close() error
}
```