package virtio

import "io"

// Block device feature bits and request types.
const (
	blkFeatSegMax = 1 << 2
	blkFeatRO     = 1 << 5
	blkFeatFlush  = 1 << 9

	blkIn    = 0
	blkOut   = 1
	blkFlush = 4
	blkGetID = 8

	blkOK          = 0
	blkErr         = 1
	blkUnsupported = 2

	blkIDLen = 20 // VIRTIO_BLK_ID_BYTES
)

// BlockFile backs a block device.
type BlockFile interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
}

// Blk is a virtio block device serving requests synchronously.
type Blk struct {
	File     BlockFile
	Sectors  uint64 // capacity in 512-byte sectors
	ReadOnly bool
	Serial   string // returned by VIRTIO_BLK_T_GET_ID; at most 20 bytes

	Requests, BytesRead, BytesWritten uint64
}

func (d *Blk) DeviceID() uint32 { return 2 }
func (d *Blk) NumQueues() int   { return 1 }
func (d *Blk) Reset()           {}

func (d *Blk) Features() uint64 {
	f := uint64(FeatVersion1 | blkFeatSegMax | blkFeatFlush)
	if d.ReadOnly {
		f |= blkFeatRO
	}
	return f
}

func (d *Blk) Config() []byte {
	cfg := make([]byte, 64)
	le.PutUint64(cfg[0:], d.Sectors)
	le.PutUint32(cfg[12:], MaxQueueSize-2) // seg_max
	return cfg
}

// Notify serves every request the driver has made available, then raises
// the interrupt once.
func (d *Blk) Notify(t *Transport, qi int) {
	q := t.Queue(qi)
	served := false
	for {
		c, ok, err := q.Pop(t.Memory())
		if !ok {
			break
		}
		var written uint32
		if err == nil {
			written = d.request(c.Buffers)
		}
		q.Push(t.Memory(), c.Head, written)
		served = true
	}
	if served {
		t.Interrupt(qi)
	}
}

// request executes one chain: header, data buffers, status byte. It returns
// the number of bytes written into guest memory.
func (d *Blk) request(chain []Buffer) uint32 {
	if len(chain) < 2 || len(chain[0].Data) < 16 {
		return 0
	}
	status := chain[len(chain)-1]
	if !status.Writable || len(status.Data) < 1 {
		return 0
	}
	kind := le.Uint32(chain[0].Data[0:])
	offset := int64(le.Uint64(chain[0].Data[8:])) * 512
	result, written := byte(blkOK), uint32(0)
	d.Requests++
	for _, buf := range chain[1 : len(chain)-1] {
		var err error
		switch {
		case kind == blkIn && buf.Writable:
			_, err = d.File.ReadAt(buf.Data, offset)
			written += uint32(len(buf.Data))
			d.BytesRead += uint64(len(buf.Data))
		case kind == blkOut && !buf.Writable && !d.ReadOnly:
			_, err = d.File.WriteAt(buf.Data, offset)
			d.BytesWritten += uint64(len(buf.Data))
		case kind == blkGetID && buf.Writable:
			id := make([]byte, blkIDLen)
			copy(id, d.Serial)
			written += uint32(copy(buf.Data, id))
		case kind == blkFlush:
		default:
			result = blkErr
		}
		if err != nil {
			result = blkErr
		}
		offset += int64(len(buf.Data))
	}
	switch kind {
	case blkIn, blkOut, blkGetID:
	case blkFlush:
		if !d.ReadOnly {
			if err := d.File.Sync(); err != nil {
				result = blkErr
			}
		}
	default:
		result = blkUnsupported
	}
	status.Data[0] = result
	return written + 1
}
