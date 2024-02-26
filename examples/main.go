package main

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"time"

	_ "embed"

	"github.com/cheggaaa/pb/v3"
	"github.com/dpeckett/noisytransport/conn"
	"github.com/dpeckett/noisytransport/transport"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const (
	nMessagesToSend = 100000
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	transportAPrivateKey, err := transport.NewPrivateKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate private key: %v\n", err)
		os.Exit(1)
	}

	transportBPrivateKey, err := transport.NewPrivateKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate private key: %v\n", err)
		os.Exit(1)
	}

	var transportAPublicKey, transportBPublicKey = transportAPrivateKey.PublicKey(), transportBPrivateKey.PublicKey()

	var transportAPort, transportBPort uint16 = 12345, 12346
	var transportA, transportB *transport.Transport

	var g errgroup.Group

	g.Go(func() error {
		sourceSink := &rickRollingSource{
			dst: transportBPublicKey,
		}

		transportA = transport.NewTransport(sourceSink, conn.NewStdNetBind(), logger)

		transportA.SetPrivateKey(transportAPrivateKey)

		if err := transportA.UpdatePort(transportAPort); err != nil {
			return fmt.Errorf("failed to update port: %v", err)
		}

		peer, err := transportA.NewPeer(transportBPublicKey)
		if err != nil {
			return fmt.Errorf("failed to create peer: %v", err)
		}

		peerAddr, err := netip.ParseAddr("127.0.0.1")
		if err != nil {
			return fmt.Errorf("failed to parse peer address: %v", err)
		}

		peer.SetEndpointFromPacket(&conn.StdNetEndpoint{
			AddrPort: netip.AddrPortFrom(peerAddr, transportBPort),
		})

		if err := transportA.Up(); err != nil {
			return fmt.Errorf("failed to bring transport up: %v", err)
		}

		<-transportA.Wait()

		return nil
	})

	g.Go(func() error {
		sourceSink := &discardingSink{
			bar: pb.StartNew(nMessagesToSend),
		}

		transportB = transport.NewTransport(sourceSink, conn.NewStdNetBind(), logger)

		transportB.SetPrivateKey(transportBPrivateKey)

		if err := transportB.UpdatePort(transportBPort); err != nil {
			return fmt.Errorf("failed to update port: %v", err)
		}

		peer, err := transportB.NewPeer(transportAPublicKey)
		if err != nil {
			return fmt.Errorf("failed to create peer: %v", err)
		}

		peerAddr, err := netip.ParseAddr("127.0.0.1")
		if err != nil {
			return fmt.Errorf("failed to parse peer address: %v", err)
		}

		peer.SetEndpointFromPacket(&conn.StdNetEndpoint{
			AddrPort: netip.AddrPortFrom(peerAddr, transportAPort),
		})

		if err := transportB.Up(); err != nil {
			return fmt.Errorf("failed to bring transport up: %v", err)
		}

		<-transportB.Wait()

		return nil
	})

	g.Go(func() error {
		term := make(chan os.Signal, 1)

		signal.Notify(term, unix.SIGTERM)
		signal.Notify(term, os.Interrupt)

		<-term

		logger.Info("Shutting down")

		_ = transportA.Close()
		_ = transportB.Close()

		return nil
	})

	if err := g.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

//go:embed messages.txt
var messages []byte

type rickRollingSource struct {
	dst           transport.NoisePublicKey
	closed        bool
	totalMessages int
}

func (r *rickRollingSource) Close() error {
	r.closed = true
	return nil
}

func (r *rickRollingSource) Read(bufs [][]byte, sizes []int, peers []transport.NoisePublicKey, offset int) (int, error) {
	if r.closed {
		return 0, net.ErrClosed
	}

	if r.totalMessages >= nMessagesToSend {
		time.Sleep(10 * time.Millisecond)
		return 0, nil
	}

	count := 0
	for i := range bufs {
		peers[i] = r.dst

		length, err := rand.Int(rand.Reader, big.NewInt(int64(transport.DefaultMTU-1-offset)))
		if err != nil {
			return 0, err
		}

		size := int(length.Int64()) + 1
		copy(bufs[i][offset:], messages[:size])
		sizes[i] = size

		r.totalMessages++
		count++

		if r.totalMessages >= nMessagesToSend {
			break
		}
	}

	time.Sleep(50 * time.Millisecond)

	return count, nil
}

func (r *rickRollingSource) Write(bufs [][]byte, peers []transport.NoisePublicKey, offset int) (int, error) {
	return 0, nil
}

func (r *rickRollingSource) BatchSize() int {
	return conn.IdealBatchSize
}

type discardingSink struct {
	closed bool
	bar    *pb.ProgressBar
}

func (ss *discardingSink) Close() error {
	ss.closed = true
	return nil
}

func (ss *discardingSink) Read(bufs [][]byte, sizes []int, peers []transport.NoisePublicKey, offset int) (int, error) {
	if ss.closed {
		return 0, net.ErrClosed
	}

	time.Sleep(10 * time.Millisecond)

	return 0, nil
}

func (ss *discardingSink) Write(bufs [][]byte, peers []transport.NoisePublicKey, offset int) (int, error) {
	ss.bar.Add(len(bufs))
	if ss.bar.Current() >= nMessagesToSend {
		ss.bar.Finish()
	}
	return 0, nil
}

func (discardingSink) BatchSize() int {
	return conn.IdealBatchSize
}
