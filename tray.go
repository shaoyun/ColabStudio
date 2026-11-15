package main

import (
	"bytes"
	"github.com/egoist/mygo"
	"image"
	"image/color"
	"image/png"
)

func setupTray(w *mygo.Window) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 7; y < 25; y++ {
		for x := 5; x < 27; x++ {
			if x == 5 || x == 26 || y == 7 || y == 24 || y >= 13 && y <= 15 && x >= 10 && x <= 21 {
				img.Set(x, y, color.NRGBA{A: 255})
			}
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	menu := mygo.NewMenu([]*mygo.MenuItem{{Label: "打开 Colab Studio", Click: func(*mygo.MenuItem, *mygo.Window) { w.Show() }}, {Label: "打开 Toybox 网页", Click: func(*mygo.MenuItem, *mygo.Window) { go func() { _ = activeStudio.OpenToybox() }() }}, mygo.Separator(), {Label: "退出（云端实例不会自动释放）", Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }}})
	tray, e := mygo.NewTray(mygo.TrayOptions{ID: "colabstudio-manager", Icon: b.Bytes(), IconIsTemplate: true, ToolTip: "Colab Studio · 本地 Colab 服务管理", Menu: menu})
	if e != nil {
		return
	}
	quitting := false
	w.OnClose(func(e *mygo.CloseEvent) {
		if !quitting {
			e.PreventDefault()
			w.Hide()
		}
	})
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true })
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) { tray.Destroy() })
}
