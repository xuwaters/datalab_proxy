package main

import (
	"context"
	"io"
)

type reader func(target []byte) (int, error)

func (f reader) Read(target []byte) (int, error) {
	return f(target)
}

// reader should implement io.Reader interface
var _ io.Reader = reader(nil)

func CtxCopy(ctx context.Context, dst io.Writer, src io.Reader) (err error) {
	_, err = io.Copy(dst, reader(func(target []byte) (int, error) {
    select {
    case <- ctx.Done():
      return 0, ctx.Err()
    default:
      return src.Read(target)
    }    
  }))
  return
}
