//go:build unix

package vmcontrol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/sys/unix"
)

// The control protocol's three protobuf messages, hand-rolled. Cortex cannot
// import moca's gen module (its modules use a local replace and no submodule
// tags are published), so these are encoded and decoded here, wire-compatible
// byte for byte with egressv1:
//
//	message ControlRequest {
//	  oneof msg {
//	    Register register = 1;   // { string vm_id = 1; string sid = 2; bytes grant = 3; }
//	    Unregister unregister = 2; // { string vm_id = 1; }
//	    reserved 3;              // Rebind, MI1 S2
//	  }
//	}
//	message ControlReply {
//	  bool ok = 1; string code = 2; string message = 3;
//	}
//
// The golden-byte tests in wire_test.go pin the encoding against bytes
// produced by moca's own proto.Marshal.

// ControlRequest is one framed request. The oneof is modelled as two pointer
// fields of which at most one is set, matching how the decoder keeps the last
// case it saw.
type ControlRequest struct {
	Register   *Register
	Unregister *Unregister
}

// Register asks the proxy to bind vmID's listener; the fd rides beside the
// frame as SCM_RIGHTS (see writeFrame).
type Register struct {
	VmID  string
	Sid   string
	Grant []byte
}

// Unregister asks the proxy to drop vmID's binding.
type Unregister struct {
	VmID string
}

// ControlReply is the server's answer to one ControlRequest.
type ControlReply struct {
	Ok      bool
	Code    string
	Message string
}

// Wire types, for skipUnknown.
const (
	wireVarint = 0
	wire64Bit  = 1
	wireBytes  = 2
	wireStart  = 3 // groups — removed in proto3, refused below
	wireEnd    = 4
	wire32Bit  = 5
)

func appendTag(b []byte, field, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// appendString appends a proto3 string field, omitting the empty value as
// proto3 marshal does.
func appendString(b []byte, field int, s string) []byte {
	if s == "" {
		return b
	}
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendBytesField(b []byte, field int, v []byte) []byte {
	if len(v) == 0 {
		return b
	}
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(v)))
	return append(b, v...)
}

func appendSubmessage(b []byte, field int, sub []byte) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(sub)))
	return append(b, sub...)
}

// Marshal encodes req. A set oneof case is emitted even when its submessage is
// empty, as proto3 does for oneof fields.
func marshalControlRequest(req *ControlRequest) []byte {
	var b []byte
	switch {
	case req.Register != nil:
		var sub []byte
		sub = appendString(sub, 1, req.Register.VmID)
		sub = appendString(sub, 2, req.Register.Sid)
		sub = appendBytesField(sub, 3, req.Register.Grant)
		b = appendSubmessage(b, 1, sub)
	case req.Unregister != nil:
		var sub []byte
		sub = appendString(sub, 1, req.Unregister.VmID)
		b = appendSubmessage(b, 2, sub)
	}
	return b
}

func marshalControlReply(rep *ControlReply) []byte {
	var b []byte
	if rep.Ok {
		b = appendTag(b, 1, wireVarint)
		b = appendVarint(b, 1)
	}
	b = appendString(b, 2, rep.Code)
	b = appendString(b, 3, rep.Message)
	return b
}

// decoder walks a protobuf message body.
type decoder struct {
	b []byte
}

func (d *decoder) more() bool { return len(d.b) > 0 }

// varint reads one base-128 value. A run longer than 10 bytes never
// terminates and is refused rather than spun on.
func (d *decoder) varint() (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		if len(d.b) == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		c := d.b[0]
		d.b = d.b[1:]
		if c < 0x80 {
			return v | uint64(c), nil
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
	}
	return 0, errors.New("vm control: unterminated varint")
}

// field reads a field key and returns the field number and wire type.
func (d *decoder) field() (num, wire int, err error) {
	key, err := d.varint()
	if err != nil {
		return 0, 0, err
	}
	num, wire = int(key>>3), int(key&7)
	if num == 0 {
		return 0, 0, errors.New("vm control: field number 0")
	}
	return num, wire, nil
}

// length reads a LEN-delimited value and returns it without copying.
func (d *decoder) length() ([]byte, error) {
	n, err := d.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.b)) {
		return nil, io.ErrUnexpectedEOF
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v, nil
}

// skipUnknown advances past one field the decoder does not know, so future
// revisions (S2's Rebind among them) do not break us. Group wire types are
// refused: proto3 removed them, and matching their end markers faithfully is
// more work than refusing a shape no encoder we interoperate with emits.
func (d *decoder) skipUnknown(wire int) error {
	switch wire {
	case wireVarint:
		_, err := d.varint()
		return err
	case wire64Bit:
		if len(d.b) < 8 {
			return io.ErrUnexpectedEOF
		}
		d.b = d.b[8:]
		return nil
	case wireBytes:
		_, err := d.length()
		return err
	case wire32Bit:
		if len(d.b) < 4 {
			return io.ErrUnexpectedEOF
		}
		d.b = d.b[4:]
		return nil
	default:
		return fmt.Errorf("vm control: unsupported wire type %d", wire)
	}
}

// decodeControlRequest parses a request body. Unknown fields are skipped;
// within the oneof, the last case seen wins, as proto3 unmarshal does.
func decodeControlRequest(body []byte) (*ControlRequest, error) {
	req := &ControlRequest{}
	d := &decoder{b: body}
	for d.more() {
		num, wire, err := d.field()
		if err != nil {
			return nil, err
		}
		if wire != wireBytes {
			if err := d.skipUnknown(wire); err != nil {
				return nil, err
			}
			continue
		}
		switch num {
		case 1:
			sub, err := d.length()
			if err != nil {
				return nil, err
			}
			reg, err := decodeRegister(sub)
			if err != nil {
				return nil, err
			}
			req.Register, req.Unregister = reg, nil
		case 2:
			sub, err := d.length()
			if err != nil {
				return nil, err
			}
			un, err := decodeUnregister(sub)
			if err != nil {
				return nil, err
			}
			req.Register, req.Unregister = nil, un
		default:
			if err := d.skipUnknown(wire); err != nil {
				return nil, err
			}
		}
	}
	return req, nil
}

func decodeRegister(body []byte) (*Register, error) {
	reg := &Register{}
	d := &decoder{b: body}
	for d.more() {
		num, wire, err := d.field()
		if err != nil {
			return nil, err
		}
		if wire != wireBytes {
			if err := d.skipUnknown(wire); err != nil {
				return nil, err
			}
			continue
		}
		v, err := d.length()
		if err != nil {
			return nil, err
		}
		switch num {
		case 1:
			reg.VmID = string(v)
		case 2:
			reg.Sid = string(v)
		case 3:
			reg.Grant = append([]byte(nil), v...)
		}
	}
	return reg, nil
}

func decodeUnregister(body []byte) (*Unregister, error) {
	un := &Unregister{}
	d := &decoder{b: body}
	for d.more() {
		num, wire, err := d.field()
		if err != nil {
			return nil, err
		}
		if wire != wireBytes {
			if err := d.skipUnknown(wire); err != nil {
				return nil, err
			}
			continue
		}
		v, err := d.length()
		if err != nil {
			return nil, err
		}
		if num == 1 {
			un.VmID = string(v)
		}
	}
	return un, nil
}

func decodeControlReply(body []byte) (*ControlReply, error) {
	rep := &ControlReply{}
	d := &decoder{b: body}
	for d.more() {
		num, wire, err := d.field()
		if err != nil {
			return nil, err
		}
		switch wire {
		case wireVarint:
			v, err := d.varint()
			if err != nil {
				return nil, err
			}
			if num == 1 {
				rep.Ok = v != 0
			}
		case wireBytes:
			v, err := d.length()
			if err != nil {
				return nil, err
			}
			switch num {
			case 2:
				rep.Code = string(v)
			case 3:
				rep.Message = string(v)
			}
		default:
			if err := d.skipUnknown(wire); err != nil {
				return nil, err
			}
		}
	}
	return rep, nil
}

var errFrameTooLarge = errors.New("vm control: frame exceeds 64 KiB")

// maxFDsPerRead bounds the fds accepted on one receive. A well-formed
// Register carries one; the headroom lets us receive and close surplus fds
// instead of the kernel dropping them silently.
const maxFDsPerRead = 8

// writeFrame sends body as one frame. If fd >= 0 it rides as SCM_RIGHTS on the
// sendmsg that carries the frame's first byte, which is the pairing readFrame
// relies on.
func writeFrame(c *net.UnixConn, body []byte, fd int) error {
	if len(body) > maxFrame {
		return errFrameTooLarge
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)

	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	n, _, err := c.WriteMsgUnix(buf, oob, nil)
	if err != nil {
		return err
	}
	// A stream sendmsg may be partial; the fd was attached to byte 0 and has
	// been sent.
	for n < len(buf) {
		m, err := c.Write(buf[n:])
		if err != nil {
			return err
		}
		n += m
	}
	return nil
}

// readFrame returns one frame body and every fd received while reading it.
// The caller owns the fds (they are raw ints, close-on-exec) and must close
// those it does not use.
func readFrame(c *net.UnixConn) (body []byte, fds []int, err error) {
	defer func() {
		if err != nil {
			closeFDs(fds)
			fds = nil
		}
	}()
	var hdr [4]byte
	if err = readFull(c, hdr[:], &fds); err != nil {
		return nil, fds, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, fds, errFrameTooLarge
	}
	body = make([]byte, n)
	if err = readFull(c, body, &fds); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fds, err
	}
	return body, fds, nil
}

// readFull fills p, collecting fds from the ancillary data of every receive,
// since the kernel may split a frame across reads and the fd arrives with
// whichever read covers its byte.
func readFull(c *net.UnixConn, p []byte, fds *[]int) error {
	oob := make([]byte, unix.CmsgSpace(4*maxFDsPerRead))
	for off := 0; off < len(p); {
		n, oobn, flags, _, err := c.ReadMsgUnix(p[off:], oob)
		if oobn > 0 {
			msgs, perr := unix.ParseSocketControlMessage(oob[:oobn])
			if perr != nil {
				return fmt.Errorf("parse control message: %w", perr)
			}
			for i := range msgs {
				rights, perr := unix.ParseUnixRights(&msgs[i])
				if perr != nil {
					continue // not SCM_RIGHTS
				}
				for _, fd := range rights {
					// net's ReadMsgUnix cannot pass MSG_CMSG_CLOEXEC, so there is a short
					// window between the receive and this call in which a concurrent
					// fork+exec could inherit the fd. Accepted: the proxy execs nothing on
					// this path.
					unix.CloseOnExec(fd)
				}
				*fds = append(*fds, rights...)
			}
		}
		if flags&unix.MSG_CTRUNC != 0 {
			return errors.New("vm control: ancillary data truncated")
		}
		off += n
		if err != nil {
			return err
		}
	}
	return nil
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}
