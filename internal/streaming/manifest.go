package streaming

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// ParseHLSManifest parses an HLS media playlist (.m3u8) and extracts chunk info.
func ParseHLSManifest(streamKey string, data []byte) (*models.ManifestInfo, error) {
	m := &models.ManifestInfo{
		StreamKey: streamKey,
		Type:      models.StreamTypeHLS,
		UpdatedAt: time.Now().Unix(),
	}

	var medSeq int64
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
				v := strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")
				if dur, err := strconv.ParseFloat(v, 64); err == nil {
					m.TargetDurMs = int64(dur * 1000)
				}
			}
			if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
				v := strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")
				if s, err := strconv.ParseInt(v, 10, 64); err == nil {
					medSeq = s
					m.MedSeq = s
				}
			}
			if line == "#EXT-X-ENDLIST" {
				m.Endlist = true
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		chunk := models.ChunkInfo{
			StreamKey: streamKey,
			SeqNum:    medSeq,
			URL:       line,
		}
		if m.TargetDurMs > 0 {
			chunk.DurationMs = m.TargetDurMs
		}
		m.Chunks = append(m.Chunks, chunk)
		medSeq++
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan m3u8: %w", err)
	}
	return m, nil
}

// ─── DASH (MPEG-DASH .mpd) parsing ────────────────────────────────

type mpdRoot struct {
	XMLName xml.Name  `xml:"MPD"`
	Periods []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	AdaptationSets []mpdAdaptationSet `xml:"AdaptationSet"`
}

type mpdAdaptationSet struct {
	ContentType     string              `xml:"contentType,attr"`
	SegmentTemplate *mpdSegmentTemplate `xml:"SegmentTemplate"`
	SegmentList     *mpdSegmentList     `xml:"SegmentList"`
}

type mpdSegmentTemplate struct {
	Timescale    int64  `xml:"timescale,attr"`
	Duration     int64  `xml:"duration,attr"`
	StartNumber  int64  `xml:"startNumber,attr"`
	Media        string `xml:"media,attr"`
	Initialization string `xml:"initialization,attr"`
}

type mpdSegmentList struct {
	SegmentURLs []mpdSegmentURL `xml:"SegmentURL"`
	Duration    int64           `xml:"duration,attr"`
	Timescale   int64           `xml:"timescale,attr"`
}

type mpdSegmentURL struct {
	Media string `xml:"media,attr"`
}

// ParseDASHManifest parses an MPEG-DASH manifest (.mpd) and extracts chunk info.
func ParseDASHManifest(streamKey string, data []byte) (*models.ManifestInfo, error) {
	var root mpdRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("unmarshal mpd: %w", err)
	}

	m := &models.ManifestInfo{
		StreamKey: streamKey,
		Type:      models.StreamTypeDASH,
		UpdatedAt: time.Now().Unix(),
	}

	for _, period := range root.Periods {
		for _, as := range period.AdaptationSets {
			if as.SegmentTemplate != nil {
				st := as.SegmentTemplate
				totalDurMs := int64(0)
				if st.Timescale > 0 && st.Duration > 0 {
					totalDurMs = st.Duration * 1000 / st.Timescale
				}
				startNum := st.StartNumber
				if startNum == 0 {
					startNum = 1
				}
				segmentCount := int64(50)
				for i := int64(0); i < segmentCount; i++ {
					seq := startNum + i
					chunk := models.ChunkInfo{
						StreamKey:  streamKey,
						SeqNum:     seq,
						URL:        fmt.Sprintf(expandDASHTemplate(st.Media, seq), seq),
						DurationMs: totalDurMs,
					}
					m.Chunks = append(m.Chunks, chunk)
				}
				if totalDurMs > 0 {
					m.TargetDurMs = totalDurMs
				}
			}
			if as.SegmentList != nil {
				sl := as.SegmentList
				startSeq := int64(1)
				for i, seg := range sl.SegmentURLs {
					totalDurMs := int64(0)
					if sl.Timescale > 0 && sl.Duration > 0 {
						totalDurMs = sl.Duration * 1000 / sl.Timescale
					}
					chunk := models.ChunkInfo{
						StreamKey:  streamKey,
						SeqNum:     startSeq + int64(i),
						URL:        seg.Media,
						DurationMs: totalDurMs,
					}
					m.Chunks = append(m.Chunks, chunk)
				}
			}
		}
	}

	return m, nil
}

func expandDASHTemplate(tpl string, num int64) string {
	r := strings.ReplaceAll(tpl, "$RepresentationID$", "1")
	r = strings.ReplaceAll(r, "$Number$", strconv.FormatInt(num, 10))
	r = strings.ReplaceAll(r, "$Time$", "0")
	return r
}

// IdentifyStreamType guesses the streaming protocol from a key or content type.
func IdentifyStreamType(key string, contentType string) models.StreamType {
	if strings.HasSuffix(strings.ToLower(key), ".m3u8") {
		return models.StreamTypeHLS
	}
	if strings.HasSuffix(strings.ToLower(key), ".mpd") {
		return models.StreamTypeDASH
	}
	if strings.Contains(strings.ToLower(contentType), "application/vnd.apple.mpegurl") ||
		strings.Contains(strings.ToLower(contentType), "application/x-mpegurl") {
		return models.StreamTypeHLS
	}
	if strings.Contains(strings.ToLower(contentType), "application/dash+xml") {
		return models.StreamTypeDASH
	}
	return ""
}
