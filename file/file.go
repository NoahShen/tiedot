/* Common data file features. */
package file

import (
	"errors"
	"fmt"
	"log"
	"loveoneanother.at/tiedot/gommap"
	"os"
)

const FILE_GROWTH_INCREMENTAL = uint64(1048576)

type File struct {
	Name                   string
	Fh                     *os.File
	UsedSize, Size, Growth uint64
	Buf                    gommap.MMap
}

// Open (create if non-exist) the file.
func Open(name string, growth uint64) (file *File, err error) {
	if growth < 1 {
		err = errors.New(fmt.Sprintf("Growth size (%d) is too small (opening %s)", growth, name))
	}
	file = &File{Name: name, Growth: growth}
	if file.Fh, err = os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600); err != nil {
		return
	}
	fsize, err := file.Fh.Seek(0, os.SEEK_END)
	if err != nil {
		return
	}
	file.Size = uint64(fsize)
	if file.Size == 0 {
		file.CheckSizeAndEnsure(file.Growth)
		return
	}

	if file.Buf, err = gommap.Map(file.Fh, gommap.RDWR, 0); err != nil {
		return
	}
	// find used size
	for low, mid, high := uint64(0), file.Size/2, file.Size; ; {
		switch {
		case high-mid == 1:
			if file.Buf[mid] == 0 {
				if file.Buf[mid-1] == 0 {
					file.UsedSize = mid - 1
				} else {
					file.UsedSize = mid
				}
				return
			}
			file.UsedSize = high
			return
		case file.Buf[mid] == 0:
			high = mid
			mid = low + (mid-low)/2
		default:
			low = mid
			mid = mid + (high-mid)/2
		}
	}
	log.Printf("%s has %d bytes out of %d bytes in-use", name, file.UsedSize, file.Size)
	return
}

// Ensure the file has room for more data.
func (file *File) CheckSize(more uint64) bool {
	return file.UsedSize+more <= file.Size
}

// Ensure the file ahs room for more data.
func (file *File) CheckSizeAndEnsure(more uint64) {
	if file.UsedSize+more <= file.Size {
		return
	}
	var err error
	if file.Buf != nil {
		if err = file.Buf.Unmap(); err != nil {
			panic(err)
		}
	}
	if _, err = file.Fh.Seek(0, os.SEEK_END); err != nil {
		panic(err)
	}
	// grow the file incrementally
	zeroBuf := make([]byte, FILE_GROWTH_INCREMENTAL)
	for i := uint64(0); i < file.Growth; i += FILE_GROWTH_INCREMENTAL {
		var slice []byte
		if i+FILE_GROWTH_INCREMENTAL > file.Growth {
			slice = zeroBuf[0 : i+FILE_GROWTH_INCREMENTAL-file.Growth]
		} else {
			slice = zeroBuf
		}
		if _, err = file.Fh.Write(slice); err != nil {
			panic(err)
		}
	}
	if err = file.Fh.Sync(); err != nil {
		panic(err)
	}
	if file.Buf, err = gommap.Map(file.Fh, gommap.RDWR, 0); err != nil {
		panic(err)
	}
	file.Size += file.Growth
	log.Printf("File %s has grown %d bytes\n", file.Name, file.Growth)
	file.CheckSizeAndEnsure(more)
}

// Synchronize mapped region with underlying storage device.
func (file *File) Flush() error {
	return file.Buf.Flush()
}

// Close the file.
func (file *File) Close() (err error) {
	if err = file.Buf.Unmap(); err != nil {
		return
	}
	return file.Fh.Close()
}
