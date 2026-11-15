//go:build !headless

package main

import (
	"errors"

	"github.com/egoist/mygo"
)

var errSecretNotFound = mygo.ErrSecretNotFound

func defaultKeyStore() keyStore {
	return mygo.Secrets.Service("dev.colabstudio.desktop")
}

func readSystemColabCLISecret() ([]byte, error) {
	return mygo.Secrets.Service("google-colab-cli").Get("oauth_token")
}

func platformKeepAwake(reason string) func() {
	if isWebServerMode() {
		return func() {}
	}
	return mygo.Power.KeepAwake(reason, false)
}

func platformOpenExternal(address string) error {
	if isWebServerMode() {
		return errors.New("当前为 Web 服务模式，请在浏览器中打开地址")
	}
	return mygo.Shell.OpenExternal(address)
}

func platformWriteClipboard(text string) {
	if isWebServerMode() {
		return
	}
	mygo.Clipboard.WriteText(text)
}

func platformOpenJSONDialog(title, message string, showHidden bool) ([]string, error) {
	if isWebServerMode() {
		return nil, errors.New("当前为 Web 服务模式，请在浏览器选择上传 JSON 文件")
	}
	return mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title:           title,
		Message:         message,
		Filters:         []mygo.FileFilter{{Name: "OAuth JSON", Extensions: []string{"json"}}},
		ShowHiddenFiles: showHidden,
	})
}
