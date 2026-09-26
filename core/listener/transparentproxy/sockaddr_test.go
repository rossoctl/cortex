package transparentproxy

import (
	"encoding/binary"
	"testing"
)

// makeSockaddrIn builds a Linux sockaddr_in (family=2) for ip a.b.c.d : port.
func makeSockaddrIn(a, b, c, d byte, port uint16) []byte {
	buf := make([]byte, 16) // sizeof(sockaddr_in)
	binary.NativeEndian.PutUint16(buf[0:2], afInet)
	binary.BigEndian.PutUint16(buf[2:4], port)
	buf[4], buf[5], buf[6], buf[7] = a, b, c, d
	return buf
}

// makeSockaddrIn6 builds a Linux sockaddr_in6 (family=10) for the given 16-byte
// address and port.
func makeSockaddrIn6(addr [16]byte, port uint16) []byte {
	buf := make([]byte, 28) // sizeof(sockaddr_in6)
	binary.NativeEndian.PutUint16(buf[0:2], afInet6)
	binary.BigEndian.PutUint16(buf[2:4], port)
	copy(buf[8:24], addr[:])
	return buf
}

func TestParseSockaddr(t *testing.T) {
	v6loop := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1} // ::1

	tests := []struct {
		name    string
		in      []byte
		want    string
		wantErr bool
	}{
		{"ipv4 https", makeSockaddrIn(10, 96, 0, 10, 443), "10.96.0.10:443", false},
		{"ipv4 high port", makeSockaddrIn(192, 168, 1, 5, 65000), "192.168.1.5:65000", false},
		{"ipv6 loopback", makeSockaddrIn6(v6loop, 8080), "[::1]:8080", false},
		{"too short", []byte{2, 0}, "", true},
		{"short sockaddr_in", []byte{2, 0, 1, 187, 10, 96}, "", true},
		{"unknown family", []byte{0xFF, 0xFF, 1, 187, 10, 96, 0, 10}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSockaddr(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseSockaddr = %q, want %q", got, tt.want)
			}
		})
	}
}
