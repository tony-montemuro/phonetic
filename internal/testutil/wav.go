// Package testutil holds helpers shared by tests in several packages.
package testutil

import (
	"encoding/binary"
	"math"
	"time"
)

// WAV returns a mono 16-bit PCM WAV file holding a 440 Hz tone of length d at
// the given sample rate: real audio, like a TTS engine returns, made without
// any external tools.
func WAV(d time.Duration, rate int) []byte {
	n := int(d.Seconds() * float64(rate))
	data := make([]byte, 2*n)
	for i := range n {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	h := make([]byte, 0, 44+len(data))
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(36+len(data)))
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1) // PCM
	h = binary.LittleEndian.AppendUint16(h, 1) // mono
	h = binary.LittleEndian.AppendUint32(h, uint32(rate))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate*2)) // byte rate
	h = binary.LittleEndian.AppendUint16(h, 2)              // block align
	h = binary.LittleEndian.AppendUint16(h, 16)             // bits per sample
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(len(data)))
	return append(h, data...)
}
