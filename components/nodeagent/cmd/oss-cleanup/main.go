// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// nodeagent-oss-cleanup is an offline, marker-aware object-family cleanup
// command. It must never run with the Node Agent ServiceAccount credentials.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	aliyunoss "github.com/aliyun/aliyun-oss-go-sdk/oss"
	bolt "go.etcd.io/bbolt"
)

var revisionPattern = regexp.MustCompile(`\.finalized\.(\d+)\.json$`)

type manifest struct {
	Endpoint     string   `json:"endpoint"`
	Bucket       string   `json:"bucket"`
	TargetID     string   `json:"target_id"`
	FamilyPrefix string   `json:"family_prefix"`
	Container    string   `json:"container"`
	MarkerKeys   []string `json:"marker_keys"`
	DataKeys     []string `json:"data_keys"`
	MarkerDigest string   `json:"marker_digest"`
	Phase        string   `json:"phase"`
}

func main() {
	endpoint := flag.String("endpoint", "", "OSS HTTPS endpoint")
	bucketName := flag.String("bucket", "", "OSS bucket")
	familyPrefix := flag.String("family-prefix", "", "object-family prefix ending at pod UID")
	container := flag.String("container", "sandbox", "container object-family name")
	targetID := flag.String("target-id", "", "expected Node Agent target ID")
	confirmDrain := flag.String("confirm-target-drained", "", "must exactly equal target-id after the operator completes target-wide drain")
	stateFile := flag.String("state-file", "", "durable local cleanup task database")
	apply := flag.Bool("apply", false, "execute the persisted plan; without this flag only plan")
	flag.Parse()
	if *endpoint == "" || *bucketName == "" || *familyPrefix == "" || *container == "" || *targetID == "" || *stateFile == "" {
		fatal(errors.New("endpoint, bucket, family-prefix, container, target-id, and state-file are required"))
	}
	normalizedPrefix, err := normalizeFamilyPrefix(*familyPrefix)
	if err != nil {
		fatal(err)
	}
	if err := validateContainer(*container); err != nil {
		fatal(err)
	}
	canonicalEndpoint, err := canonicalizeEndpoint(*endpoint)
	if err != nil {
		fatal(err)
	}
	if *apply && *confirmDrain != *targetID {
		fatal(errors.New("apply requires --confirm-target-drained to exactly match --target-id"))
	}
	accessKeyID := os.Getenv("OSS_ACCESS_KEY_ID")
	accessKeySecret := os.Getenv("OSS_ACCESS_KEY_SECRET")
	if accessKeyID == "" || accessKeySecret == "" {
		fatal(errors.New("OSS credentials are required in the environment"))
	}
	opts := []aliyunoss.ClientOption{aliyunoss.Timeout(10, 30)}
	if token := os.Getenv("OSS_SESSION_TOKEN"); token != "" {
		opts = append(opts, aliyunoss.SecurityToken(token))
	}
	client, err := aliyunoss.New(canonicalEndpoint, accessKeyID, accessKeySecret, opts...)
	if err != nil {
		fatal(err)
	}
	versioning, err := client.GetBucketVersioning(*bucketName)
	if err != nil {
		fatal(fmt.Errorf("read OSS bucket versioning: %w", err))
	}
	if versioning.Status != "" {
		fatal(fmt.Errorf("OSS bucket versioning must be disabled, got %q", versioning.Status))
	}
	bucket, err := client.Bucket(*bucketName)
	if err != nil {
		fatal(err)
	}
	db, err := bolt.Open(*stateFile, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	key := []byte(taskKey(canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container))
	plan, err := loadOrRefreshManifest(db, key, *apply, canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container, func() (manifest, error) {
		return buildManifest(bucket, canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container)
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("cleanup plan phase=%s markers=%d objects=%d digest=%s\n", plan.Phase, len(plan.MarkerKeys), len(plan.DataKeys), plan.MarkerDigest)
	if !*apply {
		return
	}
	if plan.Phase == "planned" {
		fresh, err := buildManifest(bucket, canonicalEndpoint, *bucketName, *targetID, normalizedPrefix, *container)
		if err != nil {
			fatal(err)
		}
		if fresh.MarkerDigest != plan.MarkerDigest || !sameKeys(fresh.MarkerKeys, plan.MarkerKeys) || !sameKeys(fresh.DataKeys, plan.DataKeys) {
			fatal(errors.New("object family changed since the cleanup plan was created; rerun without --apply to refresh the plan"))
		}
	}
	if err := execute(bucket, db, key, &plan); err != nil {
		fatal(err)
	}
	fmt.Println("cleanup complete")
}

func canonicalizeEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("endpoint must be an HTTPS origin without credentials, path, query, or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host, nil
}

func loadOrRefreshManifest(db *bolt.DB, key []byte, apply bool, endpoint, bucketName, targetID, familyPrefix, container string, build func() (manifest, error)) (manifest, error) {
	plan, found, err := readManifest(db, key)
	if err != nil {
		return manifest{}, err
	}
	if found {
		if err := validateManifest(plan, endpoint, bucketName, targetID, familyPrefix, container); err != nil {
			return manifest{}, err
		}
	}
	if !found || (!apply && plan.Phase == "planned") {
		plan, err = build()
		if err != nil {
			return manifest{}, err
		}
		if err := validateManifest(plan, endpoint, bucketName, targetID, familyPrefix, container); err != nil {
			return manifest{}, err
		}
		if err := writeManifest(db, key, plan); err != nil {
			return manifest{}, err
		}
	}
	return plan, nil
}

func buildManifest(bucket *aliyunoss.Bucket, endpoint, bucketName, targetID, familyPrefix, container string) (manifest, error) {
	prefix := familyPrefix + "/" + container + ".finalized."
	var markerKeys []string
	markerCursor := ""
	for {
		result, err := bucket.ListObjects(aliyunoss.Prefix(prefix), aliyunoss.Marker(markerCursor), aliyunoss.MaxKeys(1000))
		if err != nil {
			return manifest{}, err
		}
		for _, object := range result.Objects {
			if revisionPattern.MatchString(object.Key) {
				markerKeys = append(markerKeys, object.Key)
			}
		}
		if !result.IsTruncated {
			break
		}
		next, err := nextListMarker(markerCursor, result.NextMarker, result.Objects)
		if err != nil {
			return manifest{}, err
		}
		markerCursor = next
	}
	if len(markerKeys) == 0 {
		return manifest{}, errors.New("no finalization markers found")
	}
	sort.Slice(markerKeys, func(i, j int) bool { return revision(markerKeys[i]) < revision(markerKeys[j]) })
	var latest marker.Marker
	var previous marker.Marker
	h := sha256.New()
	for index, key := range markerKeys {
		if revision(key) != uint64(index+1) {
			return manifest{}, errors.New("marker revisions are not continuous")
		}
		reader, err := bucket.GetObject(key)
		if err != nil {
			return manifest{}, err
		}
		raw, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			return manifest{}, readErr
		}
		value, err := marker.Decode(raw)
		if err != nil {
			return manifest{}, fmt.Errorf("validate %s: %w", key, err)
		}
		if err := validateMarkerIdentity(value, key, targetID, familyPrefix, container, uint64(index+1)); err != nil {
			return manifest{}, err
		}
		if index > 0 {
			if err := validateCumulative(previous, value); err != nil {
				return manifest{}, fmt.Errorf("validate cumulative marker %s: %w", key, err)
			}
		}
		previous = value
		latest = value
		_, _ = h.Write(raw)
	}
	knownData := make(map[string]struct{}, len(latest.Objects))
	for _, object := range latest.Objects {
		if object.Key != expectedDataKey(familyPrefix, container, object.Generation) {
			return manifest{}, fmt.Errorf("marker object %q is outside the requested object family", object.Key)
		}
		header, err := bucket.GetObjectDetailedMeta(object.Key)
		if err != nil {
			return manifest{}, err
		}
		size, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64)
		if err != nil || size != object.Size || header.Get(aliyunoss.HTTPHeaderOssCRC64) != object.CRC64 {
			return manifest{}, fmt.Errorf("object %s no longer matches marker", object.Key)
		}
		knownData[object.Key] = struct{}{}
	}
	dataPattern := regexp.MustCompile(`^` + regexp.QuoteMeta(path.Join(familyPrefix, container)) + `(?:\.\d+)?\.log$`)
	var dataKeys []string
	dataCursor := ""
	dataPrefix := path.Join(familyPrefix, container)
	for {
		result, err := bucket.ListObjects(aliyunoss.Prefix(dataPrefix), aliyunoss.Marker(dataCursor), aliyunoss.MaxKeys(1000))
		if err != nil {
			return manifest{}, err
		}
		for _, object := range result.Objects {
			if dataPattern.MatchString(object.Key) {
				dataKeys = append(dataKeys, object.Key)
			}
		}
		if !result.IsTruncated {
			break
		}
		next, err := nextListMarker(dataCursor, result.NextMarker, result.Objects)
		if err != nil {
			return manifest{}, err
		}
		dataCursor = next
	}
	for key := range knownData {
		if !containsKey(dataKeys, key) {
			return manifest{}, fmt.Errorf("finalized object %s is missing from object family listing", key)
		}
	}
	sort.Strings(dataKeys)
	return manifest{Endpoint: endpoint, Bucket: bucketName, TargetID: targetID, FamilyPrefix: familyPrefix, Container: container, MarkerKeys: markerKeys, DataKeys: dataKeys, MarkerDigest: hex.EncodeToString(h.Sum(nil)), Phase: "planned"}, nil
}

func validateMarkerIdentity(value marker.Marker, key, targetID, familyPrefix, container string, expectedRevision uint64) error {
	expectedKey := path.Join(familyPrefix, fmt.Sprintf("%s.finalized.%d.json", container, expectedRevision))
	if path.Dir(key) != familyPrefix || key != expectedKey {
		return errors.New("marker key is outside the requested object family")
	}
	segments := strings.Split(familyPrefix, "/")
	if len(segments) < 4 {
		return errors.New("object family prefix does not contain cluster, namespace, sandbox, and pod UID")
	}
	cluster := segments[len(segments)-4]
	namespace := segments[len(segments)-3]
	sandboxID := segments[len(segments)-2]
	podUID := segments[len(segments)-1]
	resource := value.Resource
	if value.TargetID != targetID || value.Revision != expectedRevision ||
		resource.ClusterName != cluster || resource.Namespace != namespace || resource.SandboxID != sandboxID ||
		resource.PodUID != podUID || resource.Container != container ||
		value.StreamRef != "container-logs/"+podUID+"/"+container {
		return errors.New("marker identity does not match cleanup target")
	}
	return nil
}

func nextListMarker(current, serviceNext string, objects []aliyunoss.ObjectProperties) (string, error) {
	next := serviceNext
	if next == "" && len(objects) > 0 {
		next = objects[len(objects)-1].Key
	}
	if next == "" || next <= current {
		return "", errors.New("OSS listing made no progress")
	}
	return next, nil
}

func containsKey(keys []string, key string) bool {
	for _, candidate := range keys {
		if candidate == key {
			return true
		}
	}
	return false
}

func validateCumulative(previous, current marker.Marker) error {
	if previous.TargetID != current.TargetID || previous.StreamRef != current.StreamRef || previous.Resource != current.Resource || previous.CoverageStartedAt != current.CoverageStartedAt {
		return errors.New("marker identity changed between revisions")
	}
	if previous.HadDrops && !current.HadDrops {
		return errors.New("cumulative drop flag regressed")
	}
	if len(current.Objects) < len(previous.Objects) {
		return errors.New("cumulative object list shrank")
	}
	for index, object := range previous.Objects {
		if current.Objects[index] != object {
			return errors.New("previously finalized object changed")
		}
	}
	return nil
}

func expectedDataKey(familyPrefix, container string, generation uint64) string {
	name := container + ".log"
	if generation > 0 {
		name = fmt.Sprintf("%s.%d.log", container, generation)
	}
	return path.Join(familyPrefix, name)
}

func sameKeys(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func execute(bucket *aliyunoss.Bucket, db *bolt.DB, key []byte, plan *manifest) error {
	if plan.Phase == "planned" {
		plan.Phase = "deleting-markers"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase == "deleting-markers" {
		for _, objectKey := range reversedKeys(plan.MarkerKeys) {
			if err := bucket.DeleteObject(objectKey); err != nil {
				return err
			}
		}
		for _, objectKey := range plan.MarkerKeys {
			if err := assertMissing(bucket, objectKey); err != nil {
				return err
			}
		}
		plan.Phase = "markers-deleted"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase == "markers-deleted" {
		if err := assertNoMatchingObjects(bucket, plan.FamilyPrefix+"/"+plan.Container+".finalized.", markerKeyPattern(plan.FamilyPrefix, plan.Container), "finalization marker"); err != nil {
			return err
		}
		for _, objectKey := range plan.DataKeys {
			if err := bucket.DeleteObject(objectKey); err != nil {
				return err
			}
		}
		for _, objectKey := range plan.DataKeys {
			if err := assertMissing(bucket, objectKey); err != nil {
				return err
			}
		}
		if err := assertNoMatchingObjects(bucket, path.Join(plan.FamilyPrefix, plan.Container), dataKeyPattern(plan.FamilyPrefix, plan.Container), "data object"); err != nil {
			return err
		}
		plan.Phase = "objects-deleted"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase == "objects-deleted" {
		if err := assertNoMatchingObjects(bucket, plan.FamilyPrefix+"/"+plan.Container+".finalized.", markerKeyPattern(plan.FamilyPrefix, plan.Container), "finalization marker"); err != nil {
			return err
		}
		if err := assertNoMatchingObjects(bucket, path.Join(plan.FamilyPrefix, plan.Container), dataKeyPattern(plan.FamilyPrefix, plan.Container), "data object"); err != nil {
			return err
		}
		plan.Phase = "complete"
		if err := writeManifest(db, key, *plan); err != nil {
			return err
		}
	}
	if plan.Phase != "complete" {
		return fmt.Errorf("unknown cleanup phase %q", plan.Phase)
	}
	return errors.Join(
		assertNoMatchingObjects(bucket, plan.FamilyPrefix+"/"+plan.Container+".finalized.", markerKeyPattern(plan.FamilyPrefix, plan.Container), "finalization marker"),
		assertNoMatchingObjects(bucket, path.Join(plan.FamilyPrefix, plan.Container), dataKeyPattern(plan.FamilyPrefix, plan.Container), "data object"),
	)
}

func assertMissing(bucket *aliyunoss.Bucket, key string) error {
	exists, err := bucket.IsObjectExist(key)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("object %s still exists", key)
	}
	return nil
}

func assertNoMatchingObjects(bucket *aliyunoss.Bucket, prefix string, pattern *regexp.Regexp, kind string) error {
	cursor := ""
	for {
		result, err := bucket.ListObjects(aliyunoss.Prefix(prefix), aliyunoss.Marker(cursor), aliyunoss.MaxKeys(1000))
		if err != nil {
			return err
		}
		for _, object := range result.Objects {
			if pattern.MatchString(object.Key) {
				return fmt.Errorf("%s %s still exists", kind, object.Key)
			}
		}
		if !result.IsTruncated {
			return nil
		}
		next, err := nextListMarker(cursor, result.NextMarker, result.Objects)
		if err != nil {
			return err
		}
		cursor = next
	}
}

func readManifest(db *bolt.DB, key []byte) (manifest, bool, error) {
	var value manifest
	found := false
	err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("cleanup"))
		if bucket == nil {
			return nil
		}
		raw := bucket.Get(key)
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &value)
	})
	return value, found, err
}

func writeManifest(db *bolt.DB, key []byte, value manifest) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("cleanup"))
		if err != nil {
			return err
		}
		return bucket.Put(key, raw)
	})
}

func taskKey(endpoint, bucket, targetID, familyPrefix, container string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{endpoint, bucket, targetID, familyPrefix, container}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func validateManifest(value manifest, endpoint, bucket, targetID, familyPrefix, container string) error {
	if value.Endpoint != endpoint || value.Bucket != bucket || value.TargetID != targetID || value.FamilyPrefix != familyPrefix || value.Container != container {
		return errors.New("persisted cleanup manifest does not match the requested OSS object family")
	}
	switch value.Phase {
	case "planned", "deleting-markers", "markers-deleted", "objects-deleted", "complete":
	default:
		return fmt.Errorf("persisted cleanup manifest has unknown phase %q", value.Phase)
	}
	if len(value.MarkerKeys) == 0 {
		return errors.New("persisted cleanup manifest has no finalization markers")
	}
	for index, key := range value.MarkerKeys {
		expected := path.Join(familyPrefix, fmt.Sprintf("%s.finalized.%d.json", container, index+1))
		if key != expected {
			return fmt.Errorf("persisted cleanup marker key %q is not canonical", key)
		}
	}
	dataPattern := dataKeyPattern(familyPrefix, container)
	for index, key := range value.DataKeys {
		if !dataPattern.MatchString(key) || index > 0 && key <= value.DataKeys[index-1] {
			return fmt.Errorf("persisted cleanup data key %q is not canonical", key)
		}
	}
	digest, err := hex.DecodeString(value.MarkerDigest)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("persisted cleanup manifest has an invalid marker digest")
	}
	return nil
}

func markerKeyPattern(familyPrefix, container string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(familyPrefix+"/"+container+".finalized.") + `[0-9]+\.json$`)
}

func dataKeyPattern(familyPrefix, container string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(path.Join(familyPrefix, container)) + `(?:\.[0-9]+)?\.log$`)
}

func normalizeFamilyPrefix(value string) (string, error) {
	normalized := strings.Trim(value, "/")
	if normalized == "" {
		return "", errors.New("family-prefix must contain at least one non-slash path segment")
	}
	if path.Clean(normalized) != normalized {
		return "", errors.New("family-prefix must be canonical and contain no empty, dot, or parent segments")
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("family-prefix must be canonical and contain no empty, dot, or parent segments")
		}
	}
	return normalized, nil
}

func validateContainer(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return errors.New("container must be one canonical path segment")
	}
	return nil
}

func reversedKeys(keys []string) []string {
	reversed := make([]string, len(keys))
	for index := range keys {
		reversed[len(keys)-1-index] = keys[index]
	}
	return reversed
}

func revision(key string) uint64 {
	match := revisionPattern.FindStringSubmatch(path.Base(key))
	if match == nil {
		return 0
	}
	value, _ := strconv.ParseUint(match[1], 10, 64)
	return value
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "nodeagent-oss-cleanup:", err)
	os.Exit(1)
}
