package main

import (
	"bufio"
	"bytes"
	"fmt"
	"golang.org/x/image/tiff"
	"image/jpeg"
)

func ConvertTIFF(data []byte) ([]byte, error) {
	img, err := tiff.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode tiff: %v", err)
	}

	var output bytes.Buffer

	err = jpeg.Encode(bufio.NewWriter(&output), img, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to encode tiff to jpeg: %v", err)
	}

	return output.Bytes(), nil
}
