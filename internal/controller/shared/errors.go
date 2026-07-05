package shared

import "errors"

func As(err error, target any) bool {
	return errors.As(err, target)
}

func IgnoreMatching(err error, match func(error) bool) error {
	if match(err) {
		return nil
	}
	return err
}
