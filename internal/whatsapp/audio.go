package whatsapp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Chrome records the microphone as OPUS inside an MP4 (its MediaRecorder only
// encodes Opus). Meta accepts that container at upload time but refuses to
// deliver it — Cloud API audio requires AAC in MP4, or Opus in an OGG file —
// so the message comes back 'failed' with no recipient alert. These helpers
// remux OPUS-in-MP4 losslessly into an OGG voice note before upload.
const audioProbeTimeout = 20 * time.Second

// normalizeAudio turns a microphone capture into a container WhatsApp will
// actually deliver, returning the possibly-changed mime and bytes. The mimes
// that map straight onto Cloud API audio (OGG/Opus, M4A/AAC, MP3, AAC, AMR)
// pass through untouched; only OPUS-in-MP4 is converted.
func (s *Service) normalizeAudio(ctx context.Context, mime string, data []byte) (string, []byte, error) {
	if mime != "audio/mp4" || len(data) == 0 {
		return mime, data, nil
	}
	codec, err := s.probeAudioCodec(ctx, data)
	if err != nil || codec == "" {
		// Unreadable container: ship the original and let Meta surface the
		// delivery failure, rather than inventing a replacement.
		return mime, data, nil
	}
	if !strings.HasPrefix(strings.ToLower(codec), "opus") {
		// AAC-in-MP4 is a standard M4A; send it as-is.
		return mime, data, nil
	}
	out, err := s.remuxOpusMp4ToOgg(ctx, data)
	if err != nil {
		return mime, data, fmt.Errorf("microphone recording is OPUS-in-MP4 and remuxing it failed (ffmpeg needed): %w", err)
	}
	return "audio/ogg", out, nil
}

// probeAudioCodec reads the first audio track's codec via ffprobe.
func (s *Service) probeAudioCodec(ctx context.Context, data []byte) (string, error) {
	in, err := writeTempAudio(data, ".mp4")
	if err != nil {
		return "", err
	}
	defer os.Remove(in)

	ctx, cancel := context.WithTimeout(ctx, audioProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1", in)
	codec, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(codec)), nil
}

// remuxOpusMp4ToOgg copies the Opus frames out of the MP4 into an OGG
// container (the WhatsApp voice-note format) without re-encoding: bitrate and
// quality are preserved and the pass is fast.
func (s *Service) remuxOpusMp4ToOgg(ctx context.Context, data []byte) ([]byte, error) {
	in, err := writeTempAudio(data, ".mp4")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in)

	tmp, err := os.MkdirTemp("", "whatsapp-ogg-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := tmp + "/voice.ogg"

	ctx, cancel := context.WithTimeout(ctx, audioProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-loglevel", "error",
		"-i", in, "-c:a", "copy", "-f", "ogg", out)
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return os.ReadFile(out)
}

// writeTempAudio persists capture bytes for ffprobe/ffmpeg, which need a real
// seekable path rather than stdin.
func writeTempAudio(data []byte, ext string) (string, error) {
	tmp, err := os.MkdirTemp("", "whatsapp-in-*")
	if err != nil {
		return "", err
	}
	path := tmp + "/input" + ext
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
