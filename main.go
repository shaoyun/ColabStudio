package main

import (
	"embed"
	"encoding/base64"
	"github.com/egoist/mygo"
	"log"
	"strings"
)

//go:embed web/*
var assets embed.FS

var activeStudio = &Studio{}

func main() {
	mygo.Bind(activeStudio)
	mygo.App.WhenReady(func() {
		w := mygo.NewWindow(mygo.WindowOptions{Title: "Colab Studio", Width: 1120, Height: 780, TitleBarStyle: mygo.TitleBarHiddenInset})
		setupTray(w)
		activeStudio.services()
		startSmoke(w)
		p, _ := assets.ReadFile("web/index.html")
		css, _ := assets.ReadFile("web/style.css")
		js, _ := assets.ReadFile("web/app.js")
		telemetry, _ := assets.ReadFile("web/telemetry.js")
		js = append(js, append([]byte("\n"), telemetry...)...)
		icon, _ := assets.ReadFile("web/icon.png")
		page := strings.NewReplacer("/*__APP_CSS__*/", string(css), "/*__APP_JS__*/", string(js), "__APP_ICON__", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(icon)).Replace(string(p))
		w.Page().LoadHTML(page, "")
	})
	if err := mygo.App.Run(); err != nil {
		log.Print("应用启动失败")
	}
}
