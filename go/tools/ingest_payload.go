package tools

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/poma-ai/poma-cli/pkg/client"
)

const defaultIngestMaxBytes int64 = 512 << 20 // 512 MiB

// ingestMaxBytes returns the max ingest payload size in bytes.
// GRILL_INGEST_MAX_BYTES: unset uses defaultIngestMaxBytes; "0" means unlimited.
func ingestMaxBytes() int64 {
	v := strings.TrimSpace(os.Getenv("GRILL_INGEST_MAX_BYTES"))
	if v == "" {
		return defaultIngestMaxBytes
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return defaultIngestMaxBytes
	}
	if n == 0 {
		return 0
	}
	return n
}

func pathUnderAllowedPrefix(path, prefix string) (bool, error) {
	rp, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return false, err
	}
	rd, err := filepath.EvalSymlinks(filepath.Clean(prefix))
	if err != nil {
		return false, err
	}
	if rp == rd {
		return true, nil
	}
	sep := string(filepath.Separator)
	rdWithSep := rd + sep
	return strings.HasPrefix(rp, rdWithSep), nil
}

func readFileForIngest(path string) ([]byte, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil, errors.New("file_path is empty")
	}
	if !filepath.IsAbs(clean) {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve file_path: %w", err)
		}
		clean = filepath.Join(wd, clean)
	}

	if prefix := strings.TrimSpace(os.Getenv("GRILL_INGEST_ALLOWED_PREFIX")); prefix != "" {
		if !filepath.IsAbs(prefix) {
			wd, err := os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("resolve GRILL_INGEST_ALLOWED_PREFIX: %w", err)
			}
			prefix = filepath.Join(wd, prefix)
		}
		ok, err := pathUnderAllowedPrefix(clean, prefix)
		if err != nil {
			return nil, fmt.Errorf("file_path allowlist check: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("file_path must be under GRILL_INGEST_ALLOWED_PREFIX")
		}
	}

	fi, err := os.Stat(clean)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("file_path must be a regular file")
	}

	max := ingestMaxBytes()
	if max > 0 && fi.Size() > max {
		return nil, fmt.Errorf("file size %d exceeds GRILL_INGEST_MAX_BYTES (%d)", fi.Size(), max)
	}

	return os.ReadFile(clean)
}

// resolveGrillIngestPayload returns file bytes and effective basename for upload.
// Exactly one of input.FileBase64 or input.FilePath must be set.
func resolveGrillIngestPayload(input GrillIngestInput) (data []byte, filename string, err error) {
	b64 := strings.TrimSpace(input.FileBase64)
	fpath := strings.TrimSpace(input.FilePath)

	if b64 != "" && fpath != "" {
		return nil, "", errors.New("provide only one of file_base64 or file_path")
	}
	if b64 == "" && fpath == "" {
		return nil, "", errors.New("one of file_base64 or file_path is required")
	}

	max := ingestMaxBytes()

	if b64 != "" {
		if max > 0 {
			// Upper bound on decoded size from base64 length (ignores whitespace).
			if est := int64(base64.StdEncoding.DecodedLen(len(b64))); est > max {
				return nil, "", fmt.Errorf("file_base64 decodes to more than GRILL_INGEST_MAX_BYTES (%d)", max)
			}
		}
		data, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid file_base64: %w", err)
		}
		if len(data) == 0 {
			return nil, "", errors.New("decoded file_base64 is empty")
		}
		if max > 0 && int64(len(data)) > max {
			return nil, "", fmt.Errorf("decoded file exceeds GRILL_INGEST_MAX_BYTES (%d)", max)
		}
	} else {
		data, err = readFileForIngest(fpath)
		if err != nil {
			return nil, "", fmt.Errorf("read file_path: %w", err)
		}
		if len(data) == 0 {
			return nil, "", errors.New("file at file_path is empty")
		}
	}

	filename = strings.TrimSpace(input.Filename)
	if filename == "" {
		if fpath != "" {
			filename = filepath.Base(filepath.Clean(strings.TrimSpace(input.FilePath)))
		}
	}
	if filename == "" || filename == "." || filename == ".." {
		ext := guessExtensionFromContent(data)
		if ext == "" {
			return nil, "", errors.New("could not infer file extension: provide filename")
		}
		filename = "upload" + ext
	} else {
		filename = filepath.Base(filename)
	}

	return data, filename, nil
}

// HandleIngestUpload serves POST /ingest-upload in HTTP mode: raw body (octet-stream)
// or multipart field "file". Auth: same as MCP (x-api-key / Bearer / POMA_API_KEY).
// Filename: query filename=, header X-Filename, multipart filename, or upload.bin.
func HandleIngestUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := getToken(r.Context(), "")
	if token == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"missing API token (x-api-key, Authorization: Bearer, or POMA_API_KEY)"}`+"\n")
		return
	}

	max := ingestMaxBytes()
	if max > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, max)
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		mediaType = ""
	}

	var data []byte
	var filename string

	switch {
	case strings.HasPrefix(mediaType, "multipart/form-data"):
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeIngestUploadError(w, http.StatusBadRequest, err.Error())
			return
		}
		fh, hdr, err := r.FormFile("file")
		if err != nil {
			writeIngestUploadError(w, http.StatusBadRequest, "multipart form field \"file\" is required")
			return
		}
		defer fh.Close()
		data, err = io.ReadAll(fh)
		if err != nil {
			writeIngestUploadError(w, http.StatusBadRequest, err.Error())
			return
		}
		if max > 0 && int64(len(data)) > max {
			writeIngestUploadError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds GRILL_INGEST_MAX_BYTES (%d)", max))
			return
		}
		filename = filepath.Base(hdr.Filename)
		if filename == "" || filename == "." {
			filename = "upload.bin"
		}
		if filename == "upload.bin" {
			if ext := guessExtensionFromContent(data); ext != "" {
				filename = "upload" + ext
			}
		}

	default:
		data, err = io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeIngestUploadError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds GRILL_INGEST_MAX_BYTES (%d)", max))
				return
			}
			writeIngestUploadError(w, http.StatusBadRequest, err.Error())
			return
		}
		if max > 0 && int64(len(data)) > max {
			writeIngestUploadError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds GRILL_INGEST_MAX_BYTES (%d)", max))
			return
		}
		filename = filepath.Base(strings.TrimSpace(r.URL.Query().Get("filename")))
		if filename == "" || filename == "." {
			filename = filepath.Base(strings.TrimSpace(r.Header.Get("X-Filename")))
		}
		if filename == "" || filename == "." {
			filename = "upload.bin"
		}
		if filename == "upload.bin" {
			if ext := guessExtensionFromContent(data); ext != "" {
				filename = "upload" + ext
			}
		}
	}

	if len(data) == 0 {
		writeIngestUploadError(w, http.StatusBadRequest, "empty body")
		return
	}

	c := grillClient(token)
	// Falls back to POMA_PROJECT_ID env var when the header is absent,
	// allowing server-wide default project scoping for the HTTP upload endpoint.
	projectID := getProjectID(r.Header.Get("X-Project-ID"))
	body, st, err := grillIngestData(c, data, filename, projectID)
	if err != nil {
		writeIngestUploadError(w, http.StatusBadGateway, err.Error())
		return
	}
	if st != http.StatusCreated {
		writeIngestUploadError(w, st, fmt.Sprintf("grill ingest: %s", string(body)))
		return
	}

	j, err := client.ParseJob(body)
	if err != nil || j.JobID == "" {
		writeIngestUploadError(w, http.StatusBadGateway, fmt.Sprintf("could not parse job_id from response: %s", string(body)))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": j.JobID})
}

func writeIngestUploadError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
