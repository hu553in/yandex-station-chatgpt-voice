package voice

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "station_native.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

const stockSpotterModel = "/system/vendor/quasar/spotter_models/activation/yandex"

type nativeSpotter struct {
	native    *C.station_spotter
	directory string
}

func modelOverlay(stock string) (directory string, result error) {
	flags, err := os.ReadFile(filepath.Join(stock, "flags.txt"))
	if err != nil {
		return "", errors.New("cannot read stock Yandex model flags")
	}
	text := string(flags)
	for _, flag := range []string{"freq-filter-enabled", "use-tts-blocker"} {
		old := flag + " true"
		if !strings.Contains(text, old) {
			return "", errors.New("unexpected stock Yandex model flags")
		}
		text = strings.ReplaceAll(text, old, flag+" false")
	}
	directory, err = os.MkdirTemp("", "chatgpt-yandex-*")
	if err != nil {
		return "", errors.New("cannot create private Yandex model overlay")
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, removeModelOverlay(directory))
		}
	}()
	for _, file := range []string{"acoustic_model.nnet", "words.txt"} {
		if err := os.Symlink(filepath.Join(stock, file), filepath.Join(directory, file)); err != nil {
			return directory, err
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "flags.txt"), []byte(text), 0600); err != nil {
		return directory, err
	}
	return directory, nil
}

func newNativeSpotter() (*nativeSpotter, error) {
	dir, err := modelOverlay(stockSpotterModel)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStationAudio, err)
	}
	model := C.CString(dir)
	defer C.free(unsafe.Pointer(model))
	var message [512]C.char
	native := C.station_spotter_open(model, &message[0], C.size_t(len(message)))
	if native == nil {
		return nil, fmt.Errorf("%w: %w", errStationAudio, errors.Join(errors.New(C.GoString(&message[0])), removeModelOverlay(dir)))
	}
	return &nativeSpotter{native: native, directory: dir}, nil
}

func (s *nativeSpotter) feed(pcm []int16) (bool, error) {
	if len(pcm) != captureSamples {
		return false, errors.New("native spotter requires a 20ms capture frame")
	}
	var message [512]C.char
	result := C.station_spotter_feed(s.native, (*C.int16_t)(unsafe.Pointer(&pcm[0])), C.size_t(len(pcm)*2), &message[0], C.size_t(len(message)))
	if result < 0 {
		return false, fmt.Errorf("%w: %s", errStationAudio, C.GoString(&message[0]))
	}
	return result == 1, nil
}

func (s *nativeSpotter) close() error {
	if s.native == nil {
		return nil
	}
	var message [512]C.char
	result := C.station_spotter_close(s.native, &message[0], C.size_t(len(message)))
	s.native = nil
	err := removeModelOverlay(s.directory)
	if result != 0 {
		err = errors.Join(fmt.Errorf("%w: %s", errCleanup, C.GoString(&message[0])), err)
	}
	return err
}

func removeModelOverlay(directory string) error {
	if os.RemoveAll(directory) != nil {
		return fmt.Errorf("%w: cannot remove native model overlay", errCleanup)
	}
	return nil
}
