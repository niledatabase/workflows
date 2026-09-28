//go:build integration

package cachetest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/azure/azurite"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
)

const (
	azuriteImage = "mcr.microsoft.com/azure-storage/azurite:3.36.0@sha256:76b8127d608fab8287a14a4bfeb9a5502cdcffb4bf1e86f09f324ebb0e70edba"
	// RuntimeToken is the runtime token that Service accepts.
	RuntimeToken = "token"
)

type cacheEntry struct {
	blobName string
	version  string
	sequence uint64
}

// Service is a GitHub Actions cache service for tests that stores the archives in Azurite.
type Service struct {
	server          *httptest.Server
	credential      *azblob.SharedKeyCredential
	containerClient *container.Client
	mu              sync.Mutex
	uploads         map[string]cacheEntry
	entries         map[string]cacheEntry
	nextBlob        uint64
	nextSequence    uint64
	transferHost    string
}

// New starts Azurite and a Service, and stops them when the test ends.
// If networkName is not empty, Azurite joins the Docker network networkName, and the signed URLs use the address of Azurite in that network.
func New(t *testing.T, networkName string) *Service {
	t.Helper()
	ctx := t.Context()
	containerOptions := []testcontainers.ContainerCustomizer{
		azurite.WithEnabledServices(azurite.BlobService),
		testcontainers.WithCmdArgs("--skipApiVersionCheck"),
	}
	if networkName != "" {
		containerOptions = append(containerOptions, tcnetwork.WithNetworkName(nil, networkName))
	}
	azuriteContainer, err := azurite.Run(ctx, azuriteImage, containerOptions...)
	testcontainers.CleanupContainer(t, azuriteContainer)
	if err != nil {
		t.Fatalf("start Azurite: %v", err)
	}
	service := &Service{
		uploads: map[string]cacheEntry{},
		entries: map[string]cacheEntry{},
	}
	if networkName != "" {
		containerIP, err := azuriteContainer.ContainerIP(ctx)
		if err != nil {
			t.Fatalf("get Azurite container IP: %v", err)
		}
		service.transferHost = net.JoinHostPort(containerIP, "10000")
	}

	credential, err := azblob.NewSharedKeyCredential(azurite.AccountName, azurite.AccountKey)
	if err != nil {
		t.Fatalf("create Azurite credential: %v", err)
	}
	serviceURL, err := azuriteContainer.BlobServiceURL(ctx)
	if err != nil {
		t.Fatalf("get Azurite Blob service URL: %v", err)
	}
	blobService, err := azblob.NewClientWithSharedKeyCredential(serviceURL+"/"+azurite.AccountName, credential, nil)
	if err != nil {
		t.Fatalf("create Azurite Blob service client: %v", err)
	}
	service.credential = credential
	service.containerClient = blobService.ServiceClient().NewContainerClient("cache")
	if _, err := service.containerClient.Create(ctx, nil); err != nil {
		t.Fatalf("create Azurite Blob container: %v", err)
	}
	service.server = httptest.NewServer(http.HandlerFunc(service.serveHTTP))
	t.Cleanup(service.server.Close)
	return service
}

// ResultsURL returns the Results service URL of s.
func (s *Service) ResultsURL() string {
	return s.server.URL
}

func (s *Service) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+RuntimeToken {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch filepath.Base(request.URL.Path) {
	case "CreateCacheEntry":
		s.createCacheEntry(response, request)
	case "FinalizeCacheEntryUpload":
		s.finalizeCacheEntry(response, request)
	case "GetCacheEntryDownloadURL":
		s.getCacheEntryDownloadURL(response, request)
	default:
		http.NotFound(response, request)
	}
}

type createRequest struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type createResponse struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signed_upload_url"`
}

func (s *Service) createCacheEntry(response http.ResponseWriter, request *http.Request) {
	var body createRequest
	if !decodeJSONRequest(response, request, &body) {
		return
	}
	if body.Version == "" {
		http.Error(response, "empty cache version", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	_, entryExists := s.entries[body.Key]
	_, uploadExists := s.uploads[body.Key]
	if entryExists || uploadExists {
		s.mu.Unlock()
		http.Error(response, "cache entry with the same key, version, and scope already exists", http.StatusConflict)
		return
	}
	s.nextBlob++
	entry := cacheEntry{blobName: fmt.Sprintf("cache-%d", s.nextBlob), version: body.Version}
	s.uploads[body.Key] = entry
	s.mu.Unlock()

	uploadURL, err := s.signedBlobURL(entry.blobName, sas.BlobPermissions{Create: true, Write: true})
	if err != nil {
		http.Error(response, fmt.Sprintf("create upload URL: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSONResponse(response, createResponse{OK: true, SignedUploadURL: uploadURL})
}

type finalizeRequest struct {
	Key       string `json:"key"`
	SizeBytes int64  `json:"size_bytes,string"`
	Version   string `json:"version"`
}

type finalizeResponse struct {
	OK bool `json:"ok"`
}

func (s *Service) finalizeCacheEntry(response http.ResponseWriter, request *http.Request) {
	var body finalizeRequest
	if !decodeJSONRequest(response, request, &body) {
		return
	}
	s.mu.Lock()
	entry, exists := s.uploads[body.Key]
	s.mu.Unlock()
	if !exists || entry.version != body.Version {
		http.Error(response, "unknown cache upload", http.StatusBadRequest)
		return
	}
	properties, err := s.containerClient.NewBlockBlobClient(entry.blobName).GetProperties(request.Context(), nil)
	if err != nil {
		http.Error(response, fmt.Sprintf("get uploaded Blob properties: %v", err), http.StatusBadRequest)
		return
	}
	if properties.ContentLength == nil || *properties.ContentLength != body.SizeBytes {
		http.Error(response, "uploaded Blob size does not match", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	delete(s.uploads, body.Key)
	s.nextSequence++
	entry.sequence = s.nextSequence
	s.entries[body.Key] = entry
	s.mu.Unlock()
	writeJSONResponse(response, finalizeResponse{OK: true})
}

type lookupRequest struct {
	Key         string   `json:"key"`
	RestoreKeys []string `json:"restore_keys"`
	Version     string   `json:"version"`
}

type lookupResponse struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signed_download_url"`
	MatchedKey        string `json:"matched_key"`
}

func (s *Service) getCacheEntryDownloadURL(response http.ResponseWriter, request *http.Request) {
	var body lookupRequest
	if !decodeJSONRequest(response, request, &body) {
		return
	}
	matchedKey, entry, found := s.findEntry(body.Key, body.RestoreKeys, body.Version)
	if !found {
		writeJSONResponse(response, lookupResponse{})
		return
	}
	downloadURL, err := s.signedBlobURL(entry.blobName, sas.BlobPermissions{Read: true})
	if err != nil {
		http.Error(response, fmt.Sprintf("create download URL: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSONResponse(response, lookupResponse{OK: true, MatchedKey: matchedKey, SignedDownloadURL: downloadURL})
}

func (s *Service) signedBlobURL(blobName string, permissions sas.BlobPermissions) (string, error) {
	query, err := (sas.BlobSignatureValues{
		Version:       sas.Version,
		Protocol:      sas.ProtocolHTTPSandHTTP,
		ExpiryTime:    time.Now().Add(time.Hour).UTC(),
		Permissions:   permissions.String(),
		ContainerName: "cache",
		BlobName:      blobName,
	}).SignWithSharedKey(s.credential)
	if err != nil {
		return "", err
	}
	signedURL := s.containerClient.NewBlockBlobClient(blobName).URL() + "?" + query.Encode()
	if s.transferHost == "" {
		return signedURL, nil
	}
	parsedURL, err := url.Parse(signedURL)
	if err != nil {
		return "", err
	}
	parsedURL.Host = s.transferHost
	return parsedURL.String(), nil
}

func (s *Service) findEntry(key string, restoreKeys []string, version string) (string, cacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, exists := s.entries[key]; exists && entry.version == version {
		return key, entry, true
	}
	for _, prefix := range restoreKeys {
		matchedKey := ""
		matchedEntry := cacheEntry{}
		for candidateKey, candidateEntry := range s.entries {
			if candidateEntry.version == version && strings.HasPrefix(candidateKey, prefix) && candidateEntry.sequence > matchedEntry.sequence {
				matchedKey = candidateKey
				matchedEntry = candidateEntry
			}
		}
		if matchedKey != "" {
			return matchedKey, matchedEntry, true
		}
	}
	return "", cacheEntry{}, false
}

func decodeJSONRequest(response http.ResponseWriter, request *http.Request, value any) bool {
	if err := json.NewDecoder(request.Body).Decode(value); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSONResponse(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		panic(err)
	}
}
