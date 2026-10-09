package wl

import (
	"fmt"
	"image"

	"golang.org/x/sys/unix"
)

// wl_shm formats (0 and 1 are special-cased; the rest are DRM fourccs).
const (
	FmtARGB8888    uint32 = 0
	FmtXRGB8888    uint32 = 1
	FmtXBGR8888    uint32 = 0x34324258
	FmtABGR8888    uint32 = 0x34324241
	FmtXRGB2101010 uint32 = 0x30335258
	FmtARGB2101010 uint32 = 0x30335241
	FmtXBGR2101010 uint32 = 0x30334258
	FmtABGR2101010 uint32 = 0x30334241
)

// Buffer is a wl_buffer backed by a memfd we also have mapped.
type Buffer struct {
	Obj    *Object
	pool   *Object
	Data   []byte
	W, H   int
	Stride int
	Format uint32
	Dirty  image.Rectangle // pixels the last paint into it touched
	busy   chan struct{}
}

// NewBuffer allocates a shared-memory buffer.
func NewBuffer(c *Conn, shm *Object, w, h, stride int, format uint32) (*Buffer, error) {
	size := stride * h
	if size <= 0 {
		return nil, fmt.Errorf("bad buffer size %dx%d stride %d", w, h, stride)
	}
	fd, err := unix.MemfdCreate("pc-buffer", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("memfd: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		return nil, err
	}
	data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	b := &Buffer{Data: data, W: w, H: h, Stride: stride, Format: format, busy: make(chan struct{}, 1)}
	b.pool = c.NewObject("wl_shm_pool", nil)
	if err := shm.Req(0, NewMsg().U(b.pool.ID).FD(fd).I(int32(size))); err != nil {
		unix.Munmap(data)
		return nil, err
	}
	b.Obj = c.NewObject("wl_buffer", func(op uint16, e *Event) {
		if op == 0 { // release
			select {
			case <-b.busy:
			default:
			}
		}
	})
	if err := b.pool.Req(0, NewMsg().U(b.Obj.ID).I(0).I(int32(w)).I(int32(h)).I(int32(stride)).U(format)); err != nil {
		unix.Munmap(data)
		return nil, err
	}
	return b, nil
}

// MarkBusy records that the compositor holds the buffer until release.
func (b *Buffer) MarkBusy() {
	select {
	case b.busy <- struct{}{}:
	default:
	}
}

// Busy reports whether the compositor still holds it.
func (b *Buffer) Busy() bool { return len(b.busy) > 0 }

// Destroy frees the buffer, the pool and the mapping.
func (b *Buffer) Destroy() {
	if b == nil {
		return
	}
	_ = b.Obj.Req(0, NewMsg())
	_ = b.pool.Req(1, NewMsg())
	if b.Data != nil {
		_ = unix.Munmap(b.Data)
		b.Data = nil
	}
}
