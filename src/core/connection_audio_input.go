package core

import "context"

func (h *ConnectionHandler) enqueueAudio(input audioInput) error {
	select {
	case h.clientAudioQueue <- input:
		return nil
	case <-h.stopChan:
		return context.Canceled
	}
}
