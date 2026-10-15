package main

import (
	"io"
	"os"
)

const tailChunk = 64 * 1024

// tailFile copies the last n lines of src into w without reading the whole
// file, and reports how many bytes were dropped from the front. The size is
// sampled once, so lines appended concurrently by tee are simply excluded.
func tailFile(src string, w io.Writer, n int) (dropped int64, err error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	start, err := tailOffset(f, size, n)
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(w, io.NewSectionReader(f, start, size-start))
	return start, err
}

// tailOffset returns the byte offset where the last n lines begin. A
// trailing newline at EOF does not start a new (empty) line.
func tailOffset(r io.ReaderAt, size int64, n int) (int64, error) {
	if n <= 0 {
		return size, nil
	}
	buf := make([]byte, tailChunk)
	count := 0
	for pos := size; pos > 0; {
		chunk := int64(len(buf))
		if pos < chunk {
			chunk = pos
		}
		pos -= chunk
		if _, err := r.ReadAt(buf[:chunk], pos); err != nil && err != io.EOF {
			return 0, err
		}
		for i := chunk - 1; i >= 0; i-- {
			if buf[i] != '\n' || pos+i == size-1 {
				continue
			}
			count++
			if count == n {
				return pos + i + 1, nil
			}
		}
	}
	return 0, nil
}
