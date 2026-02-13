// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"fmt"
	"math/rand"
	"os"
	"sync"
)

// Fatalf prints the message and exits the program.
func Fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	fmt.Println("")
	os.Exit(1)
}

// Fatal prints the message and exits the program.
func Fatal(args ...interface{}) {
	fmt.Fprint(os.Stderr, args...)
	fmt.Println("")
	os.Exit(1)
}

var letters = []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

const letterIdxBits = 6                    // 6 bits to represent a letter index (0-63)
const letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
const letterIdxMax = 63 / letterIdxBits    // # of letter indices fitting in 63 bits

// RandBytes fills the bytes with alphabetic characters randomly.
// Optimized version that uses bit manipulation to extract multiple random
// indices from a single random number, reducing calls to rand.Int63().
func RandBytes(r *rand.Rand, b []byte) {
	// Use a more efficient algorithm that extracts multiple indices
	// from a single random number using bit masking
	for i, cache, remain := len(b)-1, r.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = r.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letters) {
			b[i] = letters[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
}

// BufPool is a bytes.Buffer pool
type BufPool struct {
	p *sync.Pool
}

// NewBufPool creates a buffer pool.
func NewBufPool() *BufPool {
	p := &sync.Pool{
		New: func() interface{} {
			return []byte(nil)
		},
	}
	return &BufPool{
		p: p,
	}
}

// Get gets a buffer.
func (b *BufPool) Get() []byte {
	buf := b.p.Get().([]byte)
	buf = buf[:0]
	return buf
}

// Put returns a buffer.
func (b *BufPool) Put(buf []byte) {
	b.p.Put(buf)
}
