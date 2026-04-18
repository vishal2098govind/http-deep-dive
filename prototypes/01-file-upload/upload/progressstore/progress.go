package progressstore

import (
	"errors"
	"fmt"
	"strconv"
)

type Progress struct {
	Err        error
	Total      uint64
	SoFar      uint64
	IsComplete bool
}

func (p *Progress) fields() []string {
	var err string
	if p.Err != nil {
		err = p.Err.Error()
	}
	return []string{
		"so_far", fmt.Sprintf("%d", p.SoFar),
		"total", fmt.Sprintf("%d", p.Total),
		"err", err,
		"is_complete", fmt.Sprintf("%v", p.IsComplete),
	}
}

func fromMap(m map[string]string) (Progress, error) {
	total, err := strconv.ParseUint(m["total"], 10, 64)
	if err != nil {
		return Progress{}, fmt.Errorf("invalid total: %w", err)
	}
	sofar, err := strconv.ParseUint(m["so_far"], 10, 64)
	if err != nil {
		return Progress{}, fmt.Errorf("invalid so_far: %w", err)
	}
	iscomp, err := strconv.ParseBool(m["is_complete"])
	if err != nil {
		return Progress{}, fmt.Errorf("invalid is_complete: %w", err)
	}

	errstr := m["err"]
	var perr error
	if errstr != "" {
		perr = errors.New(errstr)
	}

	return Progress{
		Err:        perr,
		Total:      total,
		SoFar:      sofar,
		IsComplete: iscomp,
	}, nil
}
