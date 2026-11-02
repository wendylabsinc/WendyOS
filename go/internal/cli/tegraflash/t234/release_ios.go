package t234

import "errors"

func ReleaseUSB(serial, port string) error { return errors.New("USB flashing is unavailable on iOS") }
