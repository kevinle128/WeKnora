package docparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/sourceloc"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

type hadongOCRReader struct {
	baseURL, token string
	client         *http.Client
	poll           time.Duration
}

func NewHadongOCRReader(overrides map[string]string) *hadongOCRReader {
	base := strings.TrimRight(overrides["hadong_ocr_base_url"], "/")
	if base == "" {
		base = strings.TrimRight(os.Getenv("HADONG_OCR_BASE_URL"), "/")
	}
	if base == "" {
		base = "https://hadong.protonx.co"
	}
	token := strings.TrimSpace(overrides["hadong_ocr_token"])
	if token == "" {
		token = strings.TrimSpace(os.Getenv("HADONG_OCR_TOKEN"))
	}
	return &hadongOCRReader{baseURL: base, token: token, client: utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{Timeout: 5 * time.Minute, MaxRedirects: 3}), poll: 3 * time.Second}
}

func (r *hadongOCRReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	if r.token == "" {
		return nil, fmt.Errorf("Hadong OCR token is not configured")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, req.FileName))
	h.Set("Content-Type", "application/octet-stream")
	p, err := w.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err = p.Write(req.FileContent); err != nil {
		return nil, err
	}
	w.Close()
	resp, err := r.do(ctx, http.MethodPost, "/v1/ocr/jobs", &body, w.FormDataContentType())
	if err != nil {
		return nil, err
	}
	var job struct {
		JobID     string `json:"job_id"`
		Documents []struct {
			ID string `json:"document_id"`
		} `json:"documents"`
	}
	if err = json.Unmarshal(resp, &job); err != nil {
		return nil, err
	}
	if job.JobID == "" || len(job.Documents) == 0 {
		return nil, fmt.Errorf("Hadong OCR returned no job or document id")
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.poll):
		}
		statusBody, err := r.do(ctx, http.MethodGet, "/v1/ocr/jobs/"+job.JobID, nil, "")
		if err != nil {
			return nil, err
		}
		var status struct {
			Status string `json:"status"`
		}
		if err = json.Unmarshal(statusBody, &status); err != nil {
			return nil, err
		}
		if status.Status == "failed" {
			return nil, fmt.Errorf("Hadong OCR job failed")
		}
		if status.Status == "completed" {
			break
		}
	}
	result, err := r.do(ctx, http.MethodGet, "/v1/ocr/documents/"+job.Documents[0].ID+"/result?format=markdown", nil, "")
	if err != nil {
		return nil, err
	}
	markdown := string(result)
	return &types.ReadResult{
		MarkdownContent: markdown,
		SourceBlocks:    paddleOCRVLSourceBlocks(markdown, hadongPageUnits(markdown), req.FileType),
	}, nil
}

// hadongPageMarkerRe matches the page marker Hadong writes before each page
// of its markdown, for example "<!-- Page 3 -->".
var hadongPageMarkerRe = regexp.MustCompile(`(?i)<!--\s*page\s+(\d+)\s*-->`)

// hadongPageUnits lists the markdown paragraphs with the PDF page they come
// from, read from the page markers. Hadong returns no layout boxes, so each
// locator names the page only. Text before the first marker gets no unit.
func hadongPageUnits(markdown string) []sourceloc.Unit {
	marks := hadongPageMarkerRe.FindAllStringSubmatchIndex(markdown, -1)
	var units []sourceloc.Unit
	for i, m := range marks {
		page, err := strconv.Atoi(markdown[m[2]:m[3]])
		if err != nil || page <= 0 {
			continue
		}
		end := len(markdown)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		loc := types.SourceLocator{Type: types.SourceLocatorPDF, Page: page}
		for _, para := range strings.Split(markdown[m[1]:end], "\n\n") {
			if strings.TrimSpace(para) != "" {
				// Tags are not document text: the aligner skips them in the
				// markdown, so they must not be part of the unit either.
				units = append(units, sourceloc.Unit{Text: minerUHTMLTagRe.ReplaceAllString(para, " "), Locator: loc})
			}
		}
	}
	return units
}

func (r *hadongOCRReader) do(ctx context.Context, method, path string, body *bytes.Buffer, contentType string) ([]byte, error) {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body.Bytes())
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Hadong OCR HTTP %d", resp.StatusCode)
	}
	var out bytes.Buffer
	_, err = out.ReadFrom(resp.Body)
	return out.Bytes(), err
}
