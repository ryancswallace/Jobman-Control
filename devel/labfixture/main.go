// Command labfixture prepares a synthetic, isolated Dashboard acceptance source.
// It is never a corporate directory, an AD FS substitute, or a release binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	fixtureDatabase = "jobman_dashboard_control"
	fixtureSource   = "synthetic-dashboard-lab"
	fixtureAudience = "urn:jobman:dashboard-lab:control"
	fixtureStore    = "lab-nfs"
	fixtureTarget   = "synthetic-host"
	fixtureBaseDN   = "DC=dashboard,DC=lab,DC=test"
	fixtureBindDN   = "CN=reader," + fixtureBaseDN
)

type fixtureUser struct {
	DirectoryID string `json:"directoryId"`
	Subject     string `json:"subject"`
	Name        string `json:"name"`
}
type fixtureInput struct {
	Issuer   string        `json:"issuer"`
	Audience string        `json:"audience"`
	Host     string        `json:"host"`
	Users    []fixtureUser `json:"users"`
}
type fixtureInfo struct {
	Synthetic          bool                       `json:"synthetic"`
	Version            string                     `json:"controlVersion"`
	InstanceID         string                     `json:"instanceId"`
	Endpoint           string                     `json:"endpoint"`
	Issuer             string                     `json:"issuer"`
	DelegationAudience string                     `json:"delegationAudience"`
	Namespaces         []fixtureNamespace         `json:"namespaces"`
	Identities         []domain.DirectoryIdentity `json:"identities"`
}
type fixtureNamespace struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	TargetGenerationID string   `json:"targetGenerationId"`
	JobIDs             []string `json:"jobIds"`
	ArrayID            string   `json:"arrayId,omitempty"`
	CollectionID       string   `json:"collectionId,omitempty"`
	GraphID            string   `json:"graphId,omitempty"`
}
type fixtureState struct {
	Revision int64        `json:"revision"`
	Users    []stateUser  `json:"users"`
	Groups   []stateGroup `json:"groups"`
}
type stateUser struct {
	DirectoryID string `json:"directoryId"`
	Enabled     bool   `json:"enabled"`
}
type stateGroup struct {
	ID      string   `json:"id"`
	Members []string `json:"members"`
}

func main() {
	if err := run(); err != nil {
		// Configuration can contain private DSNs. Never print wrapped driver errors.
		_, _ = fmt.Fprintln(os.Stderr, "synthetic Lab fixture failed; inspect public configuration and private service files")
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("labfixture", flag.ContinueOnError)
	root := flags.String("root", "", "absolute private fixture directory")
	input := flags.String("config", "", "public approved synthetic identities JSON")
	database := flags.String("database-url-file", "", "private TLS DSN file for jobman_dashboard_control only")
	logRoot := flags.String("log-root", "", "absolute synthetic log object root")
	if len(os.Args) < 2 {
		return errors.New("choose prepare or directory")
	}
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if !filepath.IsAbs(*root) || *root == "/" {
		return errors.New("private absolute fixture root required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[1] {
	case "prepare":
		if !filepath.IsAbs(*logRoot) || *logRoot == "/" {
			return errors.New("separate synthetic log root required")
		}
		prepareContext, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		return prepare(prepareContext, *root, *input, *database, *logRoot)
	case "directory":
		return serveDirectory(ctx, *root)
	default:
		return errors.New("unknown fixture mode")
	}
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("fixture file exceeds bound")
	}
	return data, nil
}

func readJSON(path string, value any) error {
	data, err := readBounded(path, 1<<20)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if operationErr := decoder.Decode(value); operationErr != nil {
		return operationErr
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing fixture data")
	}
	return nil
}

func writePrivate(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(data, '\n'))
}
