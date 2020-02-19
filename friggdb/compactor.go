package friggdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"

	"github.com/google/uuid"
	"github.com/grafana/frigg/friggdb/backend"
	"github.com/grafana/frigg/friggdb/encoding"
)

type compactorConfig struct {
}

const (
	inputBlocks    = 4
	outputBlocks   = 2
	chunkSizeBytes = 1024 * 1024 * 10
)

func (rw *readerWriter) blocksToCompact(tenantID string) []uuid.UUID {
	return nil
}

// jpe : this method is brittle and has weird failure conditions.  if at any point it fails it can't clean up the old blocks and just leaves them around
func (rw *readerWriter) compact(ids []uuid.UUID, tenantID string) error {
	var err error
	bookmarks := make([]*bookmark, 0, len(ids))
	blockMetas := make([]*encoding.BlockMeta, 0, len(ids))

	totalRecords := 0
	for _, id := range ids {
		index, err := rw.r.Index(id, tenantID)
		if err != nil {
			return err
		}

		totalRecords += encoding.RecordCount(index)
		bookmarks = append(bookmarks, &bookmark{
			id:    id,
			index: index,
		})

		metaBytes, err := rw.r.BlockMeta(id, tenantID)
		if err != nil {
			return err
		}

		meta := &encoding.BlockMeta{}
		err = json.Unmarshal(metaBytes, meta)
		if err != nil {
			return err
		}
		blockMetas = append(blockMetas, meta)
	}

	recordsPerBlock := (totalRecords / outputBlocks) + 1
	var currentBlock *compactorBlock

	for !allDone(bookmarks) {
		var lowestID []byte
		var lowestObject []byte

		// find lowest ID of the new object
		for _, b := range bookmarks {
			currentID, currentObject, err := currentObject(b, tenantID, rw.r)
			if err == io.EOF {
				continue
			} else if err != nil {
				return err
			}

			// todo:  right now if we run into equal ids we take the larger object in the hopes that it's a more complete trace.
			//   in the future add a callback or something that allows the owning application to make a more intelligent choice
			//   such as combining traces if they're both incomplete
			if bytes.Equal(currentID, lowestID) {
				if len(currentObject) > len(lowestObject) {
					lowestID = currentID
					lowestObject = currentObject
				}
			} else if len(lowestID) == 0 || bytes.Compare(currentID, lowestID) == -1 {
				lowestID = currentID
				lowestObject = currentObject
			}
		}

		if len(lowestID) == 0 || len(lowestObject) == 0 {
			return fmt.Errorf("failed to find a lowest object in compaction")
		}

		// make a new block if necessary
		if currentBlock == nil {
			h, err := rw.wal.NewWorkingBlock(uuid.New(), tenantID)
			if err != nil {
				return err
			}

			currentBlock, err = newCompactorBlock(h, rw.cfg.WAL.BloomFP, rw.cfg.WAL.IndexDownsample, blockMetas)
			if err != nil {
				return err
			}
		}

		// write to new block
		err = currentBlock.write(lowestID, lowestObject)
		if err != nil {
			return err
		}

		// ship block to backend if done
		if currentBlock.length() >= recordsPerBlock {
			currentMeta, err := currentBlock.meta()
			if err != nil {
				return err
			}

			currentIndex, err := currentBlock.index()
			if err != nil {
				return err
			}

			currentBloom, err := currentBlock.bloom()
			if err != nil {
				return err
			}

			err = rw.w.Write(context.TODO(), currentBlock.id(), tenantID, currentMeta, currentBloom, currentIndex, currentBlock.objectFilePath())
			if err != nil {
				return err
			}

			currentBlock.clear()
			if err != nil {
				// jpe: log?  return warning?
			}
			currentBlock = nil
		}
	}

	// mark old blocks compacted so they don't show up in polling
	for _, blockID := range ids {
		if err := rw.c.MarkBlockCompacted(blockID, tenantID); err != nil {
			// jpe: log
		}
	}

	return nil
}

func currentObject(b *bookmark, tenantID string, r backend.Reader) ([]byte, []byte, error) {
	if len(b.currentID) != 0 && len(b.currentObject) != 0 {
		return b.currentID, b.currentObject, nil
	}

	var err error
	b.currentID, b.currentObject, err = nextObject(b, tenantID, r)
	if err != nil {
		return nil, nil, err
	}

	return b.currentID, b.currentObject, nil
}

func nextObject(b *bookmark, tenantID string, r backend.Reader) ([]byte, []byte, error) {
	var err error

	// if no objects, pull objects

	if len(b.objects) == 0 {
		// if no index left, EOF
		if len(b.index) == 0 {
			return nil, nil, io.EOF
		}

		// pull next n bytes into objects
		rec := &encoding.Record{}

		var start uint64
		var length uint32

		start = math.MaxUint64
		for length < chunkSizeBytes {
			b.index = encoding.MarshalRecordAndAdvance(rec, b.index)

			if start == math.MaxUint64 {
				start = rec.Start
			}
			length += rec.Length
		}

		b.objects, err = r.Object(b.id, tenantID, start, length)
		if err != nil {
			return nil, nil, err
		}
	}

	// attempt to get next object from objects
	objectReader := bytes.NewReader(b.objects)
	id, object, err := encoding.UnmarshalObjectFromReader(objectReader)
	if err != nil {
		return nil, nil, err
	}

	// advance the objects buffer
	bytesRead := objectReader.Size() - int64(objectReader.Len())
	if bytesRead < 0 || bytesRead > int64(len(b.objects)) {
		return nil, nil, fmt.Errorf("bad object read during compaction")
	}
	b.objects = b.objects[bytesRead:]

	return id, object, nil
}

func allDone(bookmarks []*bookmark) bool {
	for _, b := range bookmarks {
		if !b.done() {
			return false
		}
	}

	return true
}
