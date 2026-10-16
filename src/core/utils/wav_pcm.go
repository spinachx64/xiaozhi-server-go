package utils

import (
	"encoding/binary"
	"fmt"
	"os"
)

// wavToPCM24k accepts PCM16 WAV containers (including additional chunks), mixes
// stereo when necessary, and normalizes to the playback pipeline's 24kHz mono.
func wavToPCM24k(path string) ([][]byte, float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("invalid WAV header")
	}
	var channels, rate int
	var pcm []byte
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size > len(data)-offset {
			return nil, 0, fmt.Errorf("truncated WAV chunk")
		}
		chunk := data[offset : offset+size]
		switch id {
		case "fmt ":
			if size < 16 || binary.LittleEndian.Uint16(chunk[:2]) != 1 || binary.LittleEndian.Uint16(chunk[14:16]) != 16 {
				return nil, 0, fmt.Errorf("WAV must contain PCM16")
			}
			channels = int(binary.LittleEndian.Uint16(chunk[2:4]))
			rate = int(binary.LittleEndian.Uint32(chunk[4:8]))
		case "data":
			pcm = append(pcm, chunk...)
		}
		offset += size + size%2
	}
	if (channels != 1 && channels != 2) || rate <= 0 || len(pcm) == 0 || len(pcm)%(2*channels) != 0 {
		return nil, 0, fmt.Errorf("invalid WAV audio parameters or samples")
	}
	samples := make([]int16, len(pcm)/(2*channels))
	for i := range samples {
		offset := i * 2 * channels
		value := int32(int16(binary.LittleEndian.Uint16(pcm[offset:])))
		if channels == 2 {
			value = (value + int32(int16(binary.LittleEndian.Uint16(pcm[offset+2:])))) / 2
		}
		samples[i] = int16(value)
	}
	if rate != 24000 {
		samples = resamplePCM(samples, rate, 24000)
	}
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return [][]byte{out}, float64(len(samples)) / 24000, nil
}
