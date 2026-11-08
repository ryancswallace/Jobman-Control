package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ryancswallace/jobman-control/internal/auth"
	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/resources"
)

func configCommand(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("config validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	kind := flags.String("kind", "", "directory, delegation or resources")
	if len(arguments) == 0 || arguments[0] != "validate" || flags.Parse(arguments[1:]) != nil || flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: jobman-control config validate --kind directory|delegation|resources FILE")
		return 2
	}
	path := flags.Arg(0)
	var err error
	switch *kind {
	case "directory":
		_, err = directory.Load(path)
	case "delegation":
		var keys []domain.DelegationKey
		keys, err = auth.LoadDelegationKeys(path)
		seen := map[string]bool{}
		for _, key := range keys {
			if err != nil {
				break
			}
			err = domain.ValidateDelegationKey(key)
			id := key.ServiceID + "\x00" + key.KeyID
			if seen[id] {
				err = errors.New("duplicate service key registration")
			}
			seen[id] = true
		}
	case "resources":
		_, err = loadResources(path)
	default:
		err = errors.New("unsupported configuration kind")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "jobman-control: configuration validation failed")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "configuration valid (referenced files and services not accessed)")
	return 0
}

func loadResources(path string) (resources.Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return resources.Document{}, errors.New("cannot open resource document")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return resources.Document{}, errors.New("resource document must be regular")
	}
	return resources.Decode(file)
}

func resourcesCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	flags := flag.NewFlagSet("resources", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("file", "", "desired resource document")
	token := flags.String("token-file", "", "protected bearer token file; enables live plan")
	ca := flags.String("ca-file", "", "additional trusted CA file")
	approval := flags.String("approve-digest", "", "reviewed document digest required by apply")
	if len(arguments) == 0 || (arguments[0] != "plan" && arguments[0] != "apply") || flags.Parse(arguments[1:]) != nil || flags.NArg() != 0 || *path == "" {
		_, _ = fmt.Fprintln(stderr, "usage: jobman-control resources plan|apply --file FILE [--token-file FILE] [--ca-file FILE] [--approve-digest SHA256]")
		return 2
	}
	document, err := loadResources(*path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "jobman-control: invalid resource document")
		return 1
	}
	digest, err := document.Digest()
	if err != nil {
		return 1
	}
	if arguments[0] == "apply" && (*token == "" || *approval != digest) {
		_, _ = fmt.Fprintln(stderr, "jobman-control: apply requires a credential file and the exact reviewed digest")
		return 2
	}
	plan := resources.OfflinePlan(document, digest)
	if *token != "" {
		var client *resources.Client
		client, err = resources.NewClient(document.Endpoint, *token, *ca)
		if err == nil {
			defer client.Close()
			if arguments[0] == "apply" {
				plan, err = client.Apply(ctx, document, digest)
			} else {
				plan, err = client.Plan(ctx, document)
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "jobman-control: resource operation failed; prior successful operations may remain applied")
		return 1
	}
	if err = json.NewEncoder(stdout).Encode(plan); err != nil {
		return 1
	}
	return 0
}
