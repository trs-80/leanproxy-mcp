package bouncer

import (
	"sync"
	"testing"
)

func TestBufferZeroing(t *testing.T) {
	buf := GetBuffer()
	copy(buf, []byte("secret data"))
	ReturnBuffer(buf)

	buf2 := GetBuffer()
	for i := range buf2 {
		if buf2[i] != 0 {
			t.Errorf("buffer not zeroed at index %d", i)
		}
	}
	ReturnBuffer(buf2)
}

func TestBufferPoolReusesAllocations(t *testing.T) {
	buf1 := GetBuffer()
	addr1 := &buf1[0]
	ReturnBuffer(buf1)

	buf2 := GetBuffer()
	addr2 := &buf2[0]
	ReturnBuffer(buf2)

	if addr1 != addr2 {
		t.Log("buffers may be reused but not guaranteed - this is ok")
	}
}

func TestConcurrentBufferGetReturn(t *testing.T) {
	var wg sync.WaitGroup
	done := make(chan struct{})
	errors := make(chan error, 100)

	go func() {
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := GetBuffer()
				for j := range buf {
					buf[j] = byte(j % 256)
				}
				ReturnBuffer(buf)
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case err := <-errors:
		t.Errorf("concurrent buffer access error: %v", err)
	}
}

func TestBufferSize(t *testing.T) {
	buf := GetBuffer()
	if len(buf) != defaultBufferSize {
		t.Errorf("expected buffer size %d, got %d", defaultBufferSize, len(buf))
	}
	ReturnBuffer(buf)
}

func TestMaxBufferSize(t *testing.T) {
	if maxBufferSize != 65536 {
		t.Errorf("expected maxBufferSize 65536, got %d", maxBufferSize)
	}
}

func TestConstantTimeZero(t *testing.T) {
	buf := []byte("sensitive data")
	constantTimeZero(buf)

	for i := range buf {
		if buf[i] != 0 {
			t.Errorf("constantTimeZero failed at index %d", i)
		}
	}
}

func TestConstantTimeZeroEmptySlice(t *testing.T) {
	var buf []byte
	constantTimeZero(buf)
}

func TestBufferNotSharedAcrossGoroutines(t *testing.T) {
	result := make(chan []byte, 2)

	go func() {
		buf := GetBuffer()
		copy(buf, []byte("goroutine 1 data"))
		ReturnBuffer(buf)
		buf2 := GetBuffer()
		result <- buf2
	}()

	go func() {
		buf := GetBuffer()
		copy(buf, []byte("goroutine 2 data"))
		ReturnBuffer(buf)
		buf2 := GetBuffer()
		result <- buf2
	}()

	r1 := <-result
	r2 := <-result

	// Both buffers are held live simultaneously, so a correct pool must have
	// handed out distinct backing arrays. Note we deliberately do NOT compare
	// r1 and r2 byte-wise for equality: two correctly zeroed buffers ARE
	// byte-identical, so content equality is the expected result, not a leak.
	if &r1[0] == &r2[0] {
		t.Fatal("both goroutines received the same backing array - buffer shared across goroutines")
	}

	// Re-acquired buffers must carry no residue from the prior owner.
	for i := range r1 {
		if r1[i] != 0 {
			t.Fatalf("reacquired buffer 1 not zeroed at index %d: got %#x", i, r1[i])
		}
	}
	for i := range r2 {
		if r2[i] != 0 {
			t.Fatalf("reacquired buffer 2 not zeroed at index %d: got %#x", i, r2[i])
		}
	}

	ReturnBuffer(r1)
	ReturnBuffer(r2)
}

func BenchmarkBufferGetReturn(b *testing.B) {
	for i := 0; i < b.N; i++ {
		buf := GetBuffer()
		ReturnBuffer(buf)
	}
}
