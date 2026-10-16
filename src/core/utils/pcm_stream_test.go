package utils

import (
	"bytes"
	"context"
	"testing"
)

func TestStreamingPCMFramesPreserveDeltaBoundaries(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 2}, 1600) // one full 60ms frame plus a partial frame
	in := make(chan []byte, 3)
	in <- pcm[:1]
	in <- pcm[1:2901]
	in <- pcm[2901:]
	close(in)
	var output [][]byte
	if err := StreamPCMFrames(context.Background(), in, "pcm", 60, func(frame []byte) error { output = append(output, append([]byte(nil), frame...)); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(output) != 2 || len(output[0]) != 2880 || len(output[1]) != 2880 {
		t.Fatal("incorrect framing")
	}
	joined := bytes.Join(output, nil)
	if !bytes.Equal(joined[:len(pcm)], pcm) || !bytes.Equal(joined[len(pcm):], make([]byte, len(joined)-len(pcm))) {
		t.Fatal("samples lost or padding inserted between deltas")
	}
}

func TestStreamingOpusAndInvalidTail(t *testing.T) {
	for _, format := range []string{"pcm", "opus"} {
		t.Run(format, func(t *testing.T) {
			in := make(chan []byte, 1)
			in <- make([]byte, 2880)
			close(in)
			var packets [][]byte
			if err := StreamPCMFrames(context.Background(), in, format, 60, func(p []byte) error { packets = append(packets, append([]byte(nil), p...)); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(packets) != 1 {
				t.Fatal(len(packets))
			}
			if format == "opus" {
				decoder, err := NewOpusDecoder(&OpusDecoderConfig{SampleRate: 24000, MaxChannels: 1})
				if err != nil {
					t.Fatal(err)
				}
				defer decoder.Close()
				decoded, err := decoder.Decode(packets[0])
				if err != nil || len(decoded) != 2880 {
					t.Fatalf("decoded=%d err=%v", len(decoded), err)
				}
			}
		})
	}
	in := make(chan []byte, 1)
	in <- []byte{1}
	close(in)
	if err := StreamPCMFrames(context.Background(), in, "pcm", 60, func([]byte) error { return nil }); err == nil {
		t.Fatal("odd PCM tail accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := StreamPCMFrames(ctx, make(chan []byte), "pcm", 60, func([]byte) error { t.Fatal("emitted after cancellation"); return nil }); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestWAVWorksForPCMAndOpusPlayback(t *testing.T) {
	path := t.TempDir() + "/speech.wav"
	pcm := bytes.Repeat([]byte{1, 0}, 1440)
	if _, err := SaveAudioToWavFile(pcm, path, 24000, 1, 16, false); err != nil {
		t.Fatal(err)
	}
	chunks, duration, err := AudioToPCMData(path)
	if err != nil || duration != 0.06 || !bytes.Equal(chunks[0], pcm) {
		t.Fatalf("duration=%v err=%v", duration, err)
	}
	frames, err := PCM16Frames(chunks, 60)
	if err != nil || len(frames) != 1 || len(frames[0]) != 2880 {
		t.Fatalf("frames=%d err=%v", len(frames), err)
	}
	packets, duration, err := AudioToOpusData(path)
	if err != nil || len(packets) != 1 || duration != 0.06 {
		t.Fatalf("packets=%d duration=%v err=%v", len(packets), duration, err)
	}
}
