package netstack

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The ICMP helper API sends echo requests without the raw socket that only
// an administrator may open.
var (
	iphlpapi       = windows.NewLazySystemDLL("iphlpapi.dll")
	icmpCreateFile = iphlpapi.NewProc("IcmpCreateFile")
	icmpSendEcho2  = iphlpapi.NewProc("IcmpSendEcho2")
)

// ipOptionInformation is IP_OPTION_INFORMATION, for 64-bit Windows.
type ipOptionInformation struct {
	TTL         uint8
	TOS         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData uintptr
}

// icmpEchoReply is ICMP_ECHO_REPLY, for 64-bit Windows.
type icmpEchoReply struct {
	Address       [4]byte
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Options       ipOptionInformation
}

const (
	ipFlagDF            = 0x02
	ipSuccess           = 0
	ipReqTimedOut       = 11010
	ipTTLExpiredTransit = 11013
)

var (
	icmpOnce   sync.Once
	icmpHandle windows.Handle
	icmpErr    error
)

// systemPinger sends echo requests through IcmpSendEcho2, unelevated. One
// handle serves every request at once, each call blocking a thread until its
// reply or its timeout.
func systemPinger() Pinger {
	return func(ctx context.Context, e Echo) (EchoReply, error) {
		icmpOnce.Do(func() {
			h, _, err := icmpCreateFile.Call()
			if windows.Handle(h) == windows.InvalidHandle {
				icmpErr = fmt.Errorf("IcmpCreateFile: %w", err)
				return
			}
			icmpHandle = windows.Handle(h)
		})
		if icmpErr != nil {
			return EchoReply{}, icmpErr
		}
		if !e.Dst.Is4() {
			return EchoReply{}, fmt.Errorf("%s is not an IPv4 address", e.Dst)
		}
		opts := ipOptionInformation{TTL: e.TTL}
		if e.DF {
			opts.Flags = ipFlagDF
		}
		timeout := e.Timeout
		if d, ok := ctx.Deadline(); ok {
			if left := time.Until(d); left < timeout {
				timeout = left
			}
		}
		if timeout <= 0 {
			return EchoReply{}, ErrNoReply
		}
		// Room for the reply, its data, an ICMP error's 8 bytes, and the
		// IO_STATUS_BLOCK the call may write.
		buf := make([]byte, int(unsafe.Sizeof(icmpEchoReply{}))+len(e.Data)+8+16)
		var data uintptr
		if len(e.Data) > 0 {
			data = uintptr(unsafe.Pointer(&e.Data[0]))
		}
		dst := e.Dst.As4()
		n, _, err := icmpSendEcho2.Call(uintptr(icmpHandle), 0, 0, 0,
			uintptr(binary.LittleEndian.Uint32(dst[:])), // IPAddr: the address's bytes in memory order
			data, uintptr(len(e.Data)), uintptr(unsafe.Pointer(&opts)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(timeout.Milliseconds()))
		if n == 0 {
			if errno, ok := err.(windows.Errno); ok && errno == ipReqTimedOut {
				return EchoReply{}, ErrNoReply
			}
			return EchoReply{}, fmt.Errorf("IcmpSendEcho2: %w", err)
		}
		r := (*icmpEchoReply)(unsafe.Pointer(&buf[0]))
		reply := EchoReply{From: netip.AddrFrom4(r.Address), TTL: r.Options.TTL}
		switch r.Status {
		case ipSuccess:
			return reply, nil
		case ipTTLExpiredTransit:
			reply.TTLExpired = true
			return reply, nil
		case ipReqTimedOut:
			return EchoReply{}, ErrNoReply
		}
		return EchoReply{}, fmt.Errorf("%s: IP status %d", e.Dst, r.Status)
	}
}
