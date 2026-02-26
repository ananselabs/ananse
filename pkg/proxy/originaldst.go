package proxy

import (
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	SO_ORIGINAL_DST = 80
	SOL_IP          = 0
)

// getOriginalDst extracts the original destination address from a TCP connection
// that has been redirected via iptables/netfilter.
func getOriginalDst(conn net.Conn) (string, error) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return "", fmt.Errorf("not a TCP connection")
	}

	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		return "", err
	}

	var destAddr string
	var opErr error

	err = rawConn.Control(func(fd uintptr) {
		// sockaddr_in is 16 bytes
		var raw [16]byte
		rawLen := uint32(len(raw))

		// Use raw getsockopt syscall
		_, _, errno := unix.Syscall6(
			unix.SYS_GETSOCKOPT,
			fd,
			uintptr(SOL_IP),
			uintptr(SO_ORIGINAL_DST),
			uintptr(unsafe.Pointer(&raw[0])),
			uintptr(unsafe.Pointer(&rawLen)),
			0,
		)
		if errno != 0 {
			opErr = fmt.Errorf("getsockopt failed: %v", errno)
			return
		}

		// Parse sockaddr_in structure (little-endian for family)
		// Offset 0-1: sin_family (uint16, little-endian on Linux)
		// Offset 2-3: sin_port (uint16, network byte order / big-endian)
		// Offset 4-7: sin_addr (IPv4 address, network byte order)
		family := binary.LittleEndian.Uint16(raw[0:2])
		if family != unix.AF_INET {
			opErr = fmt.Errorf("not AF_INET: %d", family)
			return
		}

		port := binary.BigEndian.Uint16(raw[2:4])
		ip := net.IPv4(raw[4], raw[5], raw[6], raw[7])

		destAddr = fmt.Sprintf("%s:%d", ip.String(), port)
	})

	if err != nil {
		return "", err
	}
	return destAddr, opErr
}
