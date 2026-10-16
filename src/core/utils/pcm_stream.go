package utils

import (
	"context"
	"fmt"
	"time"

	opus "github.com/AnimeAIChat/opus"
)

// StreamPCMFrames preserves sample/frame boundaries across arbitrary cloud
// deltas. Opus uses one encoder for the entire sentence; only EOF is padded.
func StreamPCMFrames(ctx context.Context, chunks <-chan []byte, format string, durationMS int, emit func([]byte) error) error {
	if durationMS != 5 && durationMS != 10 && durationMS != 20 && durationMS != 40 && durationMS != 60 {
		return fmt.Errorf("unsupported audio frame duration %d", durationMS)
	}
	if format != "pcm" && format != "opus" {
		return fmt.Errorf("unsupported audio format %q", format)
	}
	frameBytes := 24000 * durationMS / 1000 * 2
	var encoder *opus.OpusEncoder
	if format == "opus" {
		var err error
		encoder, err = opus.CreateOpusEncoder(&opus.OpusEncoderConfig{SampleRate: 24000, MaxChannels: 1, Application: opus.ApplicationVoIP, FrameDuration: time.Duration(durationMS) * time.Millisecond})
		if err != nil {
			return err
		}
		defer encoder.Close()
	}
	encode := func(frame []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if encoder == nil {
			return emit(frame)
		}
		out := make([]byte, 4096)
		n, err := encoder.Encode(frame, out)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("Opus encoder returned an empty packet")
		}
		return emit(out[:n])
	}
	var pending []byte
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-chunks:
			if !ok {
				if len(pending)%2 != 0 {
					return fmt.Errorf("incomplete PCM16 sample at EOF")
				}
				if len(pending) > 0 {
					frame := make([]byte, frameBytes)
					copy(frame, pending)
					return encode(frame)
				}
				return nil
			}
			pending = append(pending, chunk...)
			for len(pending) >= frameBytes {
				if err := encode(pending[:frameBytes]); err != nil {
					return err
				}
				pending = pending[frameBytes:]
			}
		}
	}
}

// PCM16Frames also gives file-based PCM clients correctly sized packets.
func PCM16Frames(chunks [][]byte, durationMS int) ([][]byte, error) {
	input := make(chan []byte, len(chunks))
	for _, chunk := range chunks {
		input <- chunk
	}
	close(input)
	var frames [][]byte
	err := StreamPCMFrames(context.Background(), input, "pcm", durationMS, func(frame []byte) error { frames = append(frames, append([]byte(nil), frame...)); return nil })
	return frames, err
}
