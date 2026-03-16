// seed populates a local OCI storage directory with sample Gemara content
// for development and testing. The seed data matches the complyctl mock
// OCI registry so that complyctl can be tested against gemara-content-service.
//
// Usage:
//
//	go run ./hack/seed --output /tmp/oci-store
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
)

//go:embed testdata/*.yaml
var seedData embed.FS

const (
	gemaraCatalogType  = "application/vnd.gemara.catalog.v1+yaml"
	gemaraGuidanceType = "application/vnd.gemara.guidance.v1+yaml"
	gemaraPolicyType   = "application/vnd.gemara.policy.v1+yaml"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type layerDef struct {
	mediaType string
	data      []byte
}

type artifactDef struct {
	repo   string
	tags   []string
	layers []layerDef
}

func mustLoad(path string) []byte {
	data, err := seedData.ReadFile(path)
	if err != nil {
		log.Fatalf("failed to load seed data %q: %v", path, err)
	}
	return data
}

func main() {
	var output string
	flag.StringVar(&output, "output", "/tmp/oci-store", "Output storage root directory")
	flag.Parse()

	blobRoot := filepath.Join(output, "blobs")
	dbPath := filepath.Join(output, "index.db")

	// Load seed files (flat layout, each file loaded once)
	cisGuidance := mustLoad("testdata/cis-fedora-l1-server-guidance-cis.yaml")
	cisControls := mustLoad("testdata/cis-fedora-l1-server-controls.yaml")
	cisServerPolicy := mustLoad("testdata/cis-fedora-l1-server-policy.yaml")
	cisServerTailoredPolicy := mustLoad("testdata/cis-fedora-l1-server-tailored-policy.yaml")

	cisWsCatalog := mustLoad("testdata/cis-fedora-l1-workstation-catalog.yaml")
	cisWsPolicy := mustLoad("testdata/cis-fedora-l1-workstation-policy.yaml")

	ampelCatalog := mustLoad("testdata/ampel-branch-protection-catalog.yaml")
	ampelPolicy := mustLoad("testdata/ampel-branch-protection-policy.yaml")

	artifacts := []artifactDef{
		{
			repo: "policies/cis-fedora-l1-server",
			tags: []string{"v1.0.0", "latest"},
			layers: []layerDef{
				{mediaType: gemaraGuidanceType, data: cisGuidance},
				{mediaType: gemaraCatalogType, data: cisControls},
				{mediaType: gemaraPolicyType, data: cisServerPolicy},
			},
		},
		{
			repo: "policies/cis-fedora-l1-server-tailored",
			tags: []string{"v1.0.0", "latest"},
			layers: []layerDef{
				{mediaType: gemaraGuidanceType, data: cisGuidance},
				{mediaType: gemaraCatalogType, data: cisControls},
				{mediaType: gemaraPolicyType, data: cisServerTailoredPolicy},
			},
		},
		{
			repo: "policies/cis-fedora-l1-workstation",
			tags: []string{"v1.0.0", "latest"},
			layers: []layerDef{
				{mediaType: gemaraCatalogType, data: cisWsCatalog},
				{mediaType: gemaraPolicyType, data: cisWsPolicy},
			},
		},
		{
			repo: "policies/ampel-branch-protection",
			tags: []string{"v1.0.0", "latest"},
			layers: []layerDef{
				{mediaType: gemaraCatalogType, data: ampelCatalog},
				{mediaType: gemaraPolicyType, data: ampelPolicy},
			},
		},
	}

	emptyConfig := []byte("{}")
	emptyConfigDigest := digestOf(emptyConfig)

	if err := writeBlob(blobRoot, emptyConfigDigest, emptyConfig); err != nil {
		log.Fatalf("writing empty config blob: %v", err)
	}

	os.Remove(dbPath)

	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		log.Fatalf("opening bbolt: %v", err)
	}
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		tagsBucket, err := tx.CreateBucketIfNotExists([]byte("tags"))
		if err != nil {
			return err
		}
		manifestsBucket, err := tx.CreateBucketIfNotExists([]byte("manifests"))
		if err != nil {
			return err
		}

		for _, a := range artifacts {
			layerDescs := make([]descriptor, 0, len(a.layers))
			for _, l := range a.layers {
				contentDigest := digestOf(l.data)
				if err := writeBlob(blobRoot, contentDigest, l.data); err != nil {
					return fmt.Errorf("writing blob for %s: %w", a.repo, err)
				}
				layerDescs = append(layerDescs, descriptor{
					MediaType: l.mediaType,
					Digest:    contentDigest,
					Size:      int64(len(l.data)),
				})
			}

			m := manifest{
				SchemaVersion: 2,
				MediaType:     "application/vnd.oci.image.manifest.v1+json",
				Config: descriptor{
					MediaType: "application/vnd.oci.empty.v1+json",
					Digest:    emptyConfigDigest,
					Size:      int64(len(emptyConfig)),
				},
				Layers: layerDescs,
			}

			manifestBytes, err := json.Marshal(m)
			if err != nil {
				return fmt.Errorf("marshaling manifest for %s: %w", a.repo, err)
			}

			manifestDigest := digestOf(manifestBytes)

			if err := writeBlob(blobRoot, manifestDigest, manifestBytes); err != nil {
				return fmt.Errorf("writing manifest blob for %s: %w", a.repo, err)
			}

			if err := manifestsBucket.Put([]byte(manifestDigest), manifestBytes); err != nil {
				return err
			}

			for _, tag := range a.tags {
				tagKey := a.repo + ":" + tag
				if err := tagsBucket.Put([]byte(tagKey), []byte(manifestDigest)); err != nil {
					return err
				}
				log.Printf("  %s:%s -> %s", a.repo, tag, manifestDigest[:30]+"...")
			}
		}

		return nil
	})
	if err != nil {
		log.Fatalf("populating database: %v", err)
	}

	log.Printf("Storage root ready at %s", output)
	log.Printf("Run the server with: go run ./cmd/gemara-content-service --storage-root %s --skip-tls", output)
}

func digestOf(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h)
}

func writeBlob(blobRoot, digest string, data []byte) error {
	hex := digest[len("sha256:"):]
	prefix := hex[:2]
	dir := filepath.Join(blobRoot, "sha256", prefix)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, hex), data, 0600)
}
