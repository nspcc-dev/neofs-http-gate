package uploader

import (
	"io"
	"mime/multipart"

	"go.uber.org/zap"
)

// MultipartFile provides standard ReadCloser interface and also allows one to
// get file name, it's used for multipart uploads.
type MultipartFile interface {
	io.ReadCloser
	FileName() string
}

func fetchMultipartFile(l *zap.Logger, r io.Reader, boundary string) (MultipartFile, error) {
	reader := multipart.NewReader(r, boundary)

	for {
		part, err := reader.NextPart()
		if err != nil {
			return nil, err
		}

		name := part.FormName()
		if name == "" {
			l.Debug("ignore part, empty form name")
			continue
		}

		filename := part.FileName()

		// ignore multipart/form-data values
		if filename == "" {
			l.Debug("ignore part, empty filename", zap.String("form", name))

			continue
		}

		return part, nil
	}
}
