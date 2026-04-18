package slowreader

import (
	"io"
	"time"
)

// a token-bucket
type slowReader struct {
	r              io.Reader
	tokenBal       int64
	refillRate     int64
	lastConsumedAt time.Time
	maxBurst       int64
}

func New(r io.Reader) *slowReader {
	return &slowReader{
		r:              r,
		tokenBal:       1 * 20, // 1KiB
		refillRate:     100,    // 100KiBps
		maxBurst:       1 * 20, // 1KiB
		lastConsumedAt: time.Now(),
	}
}

func (s *slowReader) Read(p []byte) (int, error) {
	elapsed := time.Since(s.lastConsumedAt).Seconds()
	newTokens := elapsed * float64(s.refillRate)
	bal := min(s.tokenBal+int64(newTokens), s.maxBurst)

	if bal >= int64(len(p)) {
		n, err := s.r.Read(p)
		if err != nil {
			return n, err
		}

		s.tokenBal -= int64(n)
		s.lastConsumedAt = time.Now()
		return n, err
	}

	if bal > 0 && bal < int64(len(p)) {
		// we cannot get more tokens
		// read less and copy
		temp := make([]byte, bal)
		n, err := s.r.Read(temp)
		if err != nil {
			return n, err
		}
		copy(p, temp[:n])
		s.tokenBal -= int64(n)
		s.lastConsumedAt = time.Now()
		return n, err
	}

	// here bal == 0
	remaining := min(int64(len(p)), s.maxBurst)

	waitTime := (float64(remaining)) / float64(s.refillRate)
	time.Sleep(time.Duration(waitTime * float64(time.Second)))

	temp := make([]byte, remaining)
	n, err := s.r.Read(temp)
	copy(p, temp[:n])

	// now during sleep, u accumulated some tokens
	// if all those accumulated tokens were consumed, then u are again left with 0 bal

	s.tokenBal = remaining - int64(n)
	s.lastConsumedAt = time.Now()

	return n, err
}
