// Copyright 2014 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package blockfile provides methods for reading packets from blockfiles
// generated by stenotype.
package blockfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	"code.google.com/p/gopacket"
	"github.com/google/stenographer/base"
	"github.com/google/stenographer/indexfile"
	"github.com/google/stenographer/query"
	"github.com/google/stenographer/stats"
	"golang.org/x/net/context"
)

// #include <linux/if_packet.h>
import "C"

var (
	v                = base.V // Verbose logging
	packetReadNanos  = stats.S.Get("packet_read_nanos")
	packetScanNanos  = stats.S.Get("packet_scan_nanos")
	packetsRead      = stats.S.Get("packets_read")
	packetsScanned   = stats.S.Get("packets_scanned")
	packetBlocksRead = stats.S.Get("packets_blocks_read")
)

// BlockFile provides an interface to a single stenotype file on disk and its
// associated index.
type BlockFile struct {
	name string
	f    *os.File
	i    *indexfile.IndexFile
	mu   sync.RWMutex // Stops Close() from invalidating a file before a current query is done with it.
	done chan struct{}
}

// NewBlockFile opens up a named block file (and its index), returning a handle
// which can be used to look up packets.
func NewBlockFile(filename string) (*BlockFile, error) {
	v(1, "Blockfile opening: %q", filename)
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("could not open %q: %v", filename, err)
	}
	i, err := indexfile.NewIndexFile(indexfile.IndexPathFromBlockfilePath(filename))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("could not open index for %q: %v", filename, err)
	}
	return &BlockFile{
		f:    f,
		i:    i,
		name: filename,
		done: make(chan struct{}),
	}, nil
}

// Name returns the name of the file underlying this blockfile.
func (b *BlockFile) Name() string {
	return b.name
}

// readPacket reads a single packet from the file at the given position.
// It updates the passed in CaptureInfo with information on the packet.
func (b *BlockFile) readPacket(pos int64, ci *gopacket.CaptureInfo) ([]byte, error) {
	// 28 bytes actually isn't the entire packet header, but it's all the fields
	// that we care about.
	packetsRead.Increment()
	defer packetReadNanos.NanoTimer()()
	var dataBuf [28]byte
	_, err := b.f.ReadAt(dataBuf[:], pos)
	if err != nil {
		return nil, err
	}
	pkt := (*C.struct_tpacket3_hdr)(unsafe.Pointer(&dataBuf[0]))
	*ci = gopacket.CaptureInfo{
		Timestamp:     time.Unix(int64(pkt.tp_sec), int64(pkt.tp_nsec)),
		Length:        int(pkt.tp_len),
		CaptureLength: int(pkt.tp_snaplen),
	}
	out := make([]byte, ci.CaptureLength)
	pos += int64(pkt.tp_mac)
	_, err = b.f.ReadAt(out, pos)
	return out, err
}

// Close cleans up this blockfile.
func (b *BlockFile) Close() (err error) {
	v(2, "Blockfile closing: %q", b.name)
	close(b.done)
	b.mu.Lock()
	defer b.mu.Unlock()
	v(3, "Blockfile closing file descriptors: %q", b.name)
	if e := b.i.Close(); e != nil {
		err = e
	}
	if e := b.f.Close(); e != nil {
		err = e
	}
	b.i, b.f = nil, nil
	return
}

// allPacketsIter implements Iter.
type allPacketsIter struct {
	*BlockFile
	blockData        [1 << 20]byte
	block            *C.struct_tpacket_hdr_v1
	pkt              *C.struct_tpacket3_hdr
	blockPacketsRead int
	blockOffset      int64
	packetOffset     int // offset of packet in block
	err              error
	done             bool
}

func (a *allPacketsIter) Next() bool {
	defer packetScanNanos.NanoTimer()()
	if a.err != nil || a.done {
		return false
	}
	for a.block == nil || a.blockPacketsRead == int(a.block.num_pkts) {
		packetBlocksRead.Increment()
		_, err := a.f.ReadAt(a.blockData[:], a.blockOffset)
		if err == io.EOF {
			a.done = true
			return false
		} else if err != nil {
			a.err = fmt.Errorf("could not read block at %v: %v", a.blockOffset, err)
			return false
		}
		baseHdr := (*C.struct_tpacket_block_desc)(unsafe.Pointer(&a.blockData[0]))
		a.block = (*C.struct_tpacket_hdr_v1)(unsafe.Pointer(&baseHdr.hdr[0]))
		a.blockOffset += 1 << 20
		a.blockPacketsRead = 0
		a.pkt = nil
	}
	a.blockPacketsRead++
	if a.pkt == nil {
		a.packetOffset = int(a.block.offset_to_first_pkt)
	} else if a.pkt.tp_next_offset != 0 {
		a.packetOffset += int(a.pkt.tp_next_offset)
	} else {
		a.err = errors.New("block format currently not supported")
		return false
	}
	a.pkt = (*C.struct_tpacket3_hdr)(unsafe.Pointer(&a.blockData[a.packetOffset]))
	packetsScanned.Increment()
	return true
}

func (a *allPacketsIter) Packet() *base.Packet {
	start := a.packetOffset + int(a.pkt.tp_mac)
	buf := a.blockData[start : start+int(a.pkt.tp_snaplen)]
	p := &base.Packet{Data: buf}
	p.CaptureInfo.Timestamp = time.Unix(int64(a.pkt.tp_sec), int64(a.pkt.tp_nsec))
	p.CaptureInfo.Length = int(a.pkt.tp_len)
	p.CaptureInfo.CaptureLength = int(a.pkt.tp_snaplen)
	return p
}

func (a *allPacketsIter) Err() error {
	return a.err
}

// AllPackets returns a packet channel to which all packets in the blockfile are
// sent.
func (b *BlockFile) AllPackets() *base.PacketChan {
	b.mu.RLock()
	c := base.NewPacketChan(100)
	go func() {
		defer b.mu.RUnlock()
		pkts := &allPacketsIter{BlockFile: b}
		for pkts.Next() {
			c.Send(pkts.Packet())
		}
		c.Close(pkts.Err())
	}()
	return c
}

// Positions returns the positions in the blockfile of all packets matched by
// the passed-in query.
func (b *BlockFile) Positions(ctx context.Context, q query.Query) (base.Positions, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.positionsLocked(ctx, q)
}

// positionsLocked returns the positions in the blockfile of all packets matched by
// the passed-in query.  b.mu must be locked.
func (b *BlockFile) positionsLocked(ctx context.Context, q query.Query) (base.Positions, error) {
	if b.i == nil || b.f == nil {
		// If we're closed, just return nothing.
		return nil, nil
	}
	return q.LookupIn(ctx, b.i)
}

// Lookup returns all packets in the blockfile matched by the passed-in query.
func (b *BlockFile) Lookup(ctx context.Context, q query.Query, out *base.PacketChan) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var ci gopacket.CaptureInfo
	v(2, "Blockfile %q looking up query %q", q.String(), b.name)
	start := time.Now()
	positions, err := b.positionsLocked(ctx, q)
	if err != nil {
		out.Close(fmt.Errorf("index lookup failure: %v", err))
		return
	}
	if positions.IsAllPositions() {
		v(2, "Blockfile %q reading all packets", b.name)
		iter := &allPacketsIter{BlockFile: b}
	all_packets_loop:
		for iter.Next() {
			select {
			case <-ctx.Done():
				v(2, "Blockfile %q canceling packet read", b.name)
				break all_packets_loop
			case <-b.done:
				v(2, "Blockfile %q closing, breaking out of query", b.name)
				break all_packets_loop
			case out.C <- iter.Packet():
			}
		}
		if iter.Err() != nil {
			out.Close(fmt.Errorf("error reading all packets from %q: %v", b.name, iter.Err()))
			return
		}
	} else {
		v(2, "Blockfile %q reading %v packets", b.name, len(positions))
	query_packets_loop:
		for _, pos := range positions {
			buffer, err := b.readPacket(pos, &ci)
			if err != nil {
				v(2, "Blockfile %q error reading packet: %v", b.name, err)
				out.Close(fmt.Errorf("error reading packets from %q @ %v: %v", b.name, pos, err))
				return
			}
			select {
			case <-ctx.Done():
				v(2, "Blockfile %q canceling packet read", b.name)
				break query_packets_loop
			case <-b.done:
				v(2, "Blockfile %q closing, breaking out of query", b.name)
				break query_packets_loop
			case out.C <- &base.Packet{Data: buffer, CaptureInfo: ci}:
			}
		}
	}
	v(2, "Blockfile %q finished reading all packets in %v", b.name, time.Since(start))
	out.Close(ctx.Err())
}

// DumpIndex dumps out a "human-readable" debug version of the blockfile's index
// to the given writer.
func (b *BlockFile) DumpIndex(out io.Writer, start, finish []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.i.Dump(out, start, finish)
}
