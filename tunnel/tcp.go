package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/common/pool"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	"github.com/xjasonlyu/tun2socks/v2/log"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"github.com/xjasonlyu/tun2socks/v2/tunnel/statistic"
)

const (
	tcpWaitTimeout = 5 * time.Second
)

func newTCPTracker(conn net.Conn, metadata *M.Metadata) net.Conn {
	return statistic.NewTCPTracker(conn, metadata, statistic.DefaultManager)
}

func handleTCPConn(localConnSYN adapter.TCPConnSYN) {
	id := localConnSYN.ID()
	metadata := &M.Metadata{
		Network: M.TCP,
		SrcIP:   net.IP(id.RemoteAddress),
		SrcPort: id.RemotePort,
		DstIP:   net.IP(id.LocalAddress),
		DstPort: id.LocalPort,
	}

	targetConn, err := proxy.Dial(metadata)
	if err != nil {
		if _, ok := err.(*proxy.UnreachableError); !ok {
			localConnSYN.RST()
		}
		log.Warnf("[TCP] dial %s: %v", metadata.DestinationAddress(), err)
		return
	}
	metadata.MidIP, metadata.MidPort = parseAddr(targetConn.LocalAddr())

	targetConn = newTCPTracker(targetConn, metadata)
	defer targetConn.Close()

	conn, err := localConnSYN.CompleteHandshake()
	if err != nil {
		log.Warnf("[TCP] localConnSYN CompleteHandshake %s: %v", metadata.DestinationAddress(), err)
		return
	}
	defer conn.Close()

	log.Infof("[TCP] %s <-> %s", metadata.SourceAddress(), metadata.DestinationAddress())
	relay(conn, targetConn) /* relay connections */
}

// relay copies between left and right bidirectionally.
func relay(left, right net.Conn) {
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := copyBuffer(right, left); err != nil {
			log.Warnf("[TCP] %v", err)
		}
		right.SetReadDeadline(time.Now().Add(tcpWaitTimeout))
	}()

	go func() {
		defer wg.Done()
		if err := copyBuffer(left, right); err != nil {
			log.Warnf("[TCP] %v", err)
		}
		left.SetReadDeadline(time.Now().Add(tcpWaitTimeout))
	}()

	wg.Wait()
}

func copyBuffer(dst io.Writer, src io.Reader) error {
	buf := pool.Get(pool.RelayBufferSize)
	defer pool.Put(buf)

	_, err := io.CopyBuffer(dst, src, buf)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return nil /* ignore I/O timeout */
	} else if errors.Is(err, syscall.EPIPE) {
		return nil /* ignore broken pipe */
	} else if errors.Is(err, syscall.ECONNRESET) {
		return nil /* ignore connection reset by peer */
	}
	return err
}
