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

package ycsb

import (
	"errors"
	"testing"
)

func TestBatchError(t *testing.T) {
	oom := errors.New("OOM")
	if err := NewBatchError([]error{nil, nil}); err != nil {
		t.Fatalf("NewBatchError of no failure = %v, want nil", err)
	}
	err := NewBatchError([]error{nil, oom, nil, oom})
	var be *BatchError
	if !errors.As(err, &be) || be.Failed() != 2 {
		t.Fatalf("NewBatchError = %v, want a *BatchError with 2 failed", err)
	}
	if !errors.Is(err, oom) {
		t.Error("errors.Is doesn't see a record's error")
	}
	if want := "2 of 4 records failed, the first: OOM"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}

}
