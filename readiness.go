package main

import (
	"errors"
	"github.com/egoist/mygo"
	"time"
)

var copyToyboxAddress = mygo.Clipboard.WriteText

// The address is public; passwords and proxy credentials never enter the URL.
func (s *Studio) CopyToyboxURL() error {
	m := s.services()
	r := m.get()
	if r.Runtime == "" || r.RemoteRoot == "" {
		return errors.New("服务尚未生成访问地址，请等待网页连接启动")
	}
	if _, e := m.identity(r); e != nil {
		return e
	}
	state, e := m.state(r)
	if e != nil {
		return e
	}
	if !readinessFromState(state, time.Now()).Fresh {
		return errors.New("服务状态已过期，请刷新后再复制访问地址")
	}
	address, e := urlForToybox(state.URL)
	if e != nil {
		return e
	}
	current := m.get()
	if current.Runtime != r.Runtime || current.Binding != r.Binding {
		return errors.New("服务或账号已变更，请刷新后再复制地址")
	}
	copyToyboxAddress(address)
	return nil
}
