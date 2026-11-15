//go:build headless

package main

import "errors"

var errSecretNotFound = errors.New("secret not found")

func defaultKeyStore() keyStore {
	return newFileKeyStore(dataDir())
}

func readSystemColabCLISecret() ([]byte, error) {
	return nil, errSecretNotFound
}

func platformKeepAwake(string) func() {
	return func() {}
}

func platformOpenExternal(string) error {
	return errors.New("当前为无头服务端模式，请在浏览器中直接打开地址")
}

func platformWriteClipboard(string) {}

func platformOpenJSONDialog(string, string, bool) ([]string, error) {
	return nil, errors.New("当前为无头服务端模式，请通过网页上传 JSON 配置")
}
