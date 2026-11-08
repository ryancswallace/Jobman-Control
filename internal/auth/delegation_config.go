package auth

import (
	"errors"
	"io"
	"os"

	"github.com/ryancswallace/jobman-control/internal/configjson"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

// LoadDelegationKeys reads public operator registration only. Secret signing
// keys are neither accepted by this format nor stored by Control.
func LoadDelegationKeys(path string) ([]domain.DelegationKey, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open public delegation registry")
	}
	defer file.Close()
	return decodeDelegationKeys(file)
}

func decodeDelegationKeys(reader io.Reader) ([]domain.DelegationKey, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, errors.New("public delegation registry is unreadable or too large")
	}
	var document struct {
		Services []domain.DelegationKey `json:"services"`
	}
	if err = configjson.Decode(data, &document); err != nil {
		return nil, errors.New("public delegation registry is invalid")
	}
	if document.Services == nil || len(document.Services) > 64 {
		return nil, errors.New("public delegation registry needs a bounded services list")
	}
	return document.Services, nil
}
