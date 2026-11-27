package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

// Run in the real native WebView, not a mocked bridge.
func (h *host) smoke() error {
	h.mu.Lock()
	w := h.windows[h.main]
	h.mu.Unlock()
	if w == nil {
		return errors.New("主窗口已关闭")
	}
	id := w.controller
	deadline := time.Now().Add(20 * time.Second)
	for {
		source, ok := jade.GetWebViewURL(id)
		if ok && h.viewPageAllowed(id, source, "/index.html") {
			r, err := resultOf(jade.ExecuteJavaScript(id, "typeof window.jade?.invoke"))
			if err == nil && r == `"function"` {
				break
			}
		}
		if time.Now().After(deadline) {
			return errors.New("真实页面未提供 window.jade.invoke")
		}
		time.Sleep(100 * time.Millisecond)
	}
	const script = `(() => {
 window.__goSmoke = "";
 const rpc = async (action, args = {}) => {
   let r = await window.jade.invoke("playground", {action, ...args});
   if (typeof r === "string") r = JSON.parse(r);
   if (!r.ok) throw Error(r.error);
   return r.result;
 };
 (async () => {
   const hello = await rpc("hello");
   if (hello.host.language !== "go" || hello.sourceId === hello.windowId)
       throw Error("bad hello");
   if (!hello.host.name.includes(" · JadeViewGo") || hello.host.version)
       throw Error("bad host label: " + JSON.stringify(hello.host));
   const hostLabel = "当前宿主：" + hello.host.name;
   for (let i = 0; i < 30; i++) {
       if (document.querySelector(".pg-host-badge")?.textContent?.trim() === hostLabel)
           break;
       if (i === 29) throw Error("host badge did not render: " + hostLabel);
       await new Promise(resolve => setTimeout(resolve, 100));
   }
   console.log(hostLabel);
   const echo = await rpc("echo", {value:"宿主通信"});
   if (echo.value !== "宿主通信" || echo.webviewId !== hello.sourceId)
       throw Error("bad IPC echo");
   const added = await rpc("pane.add", {detached:true});
   const pane = added.panes[0];
   if (!pane || pane.parentId !== hello.windowId) throw Error("missing workspace view");
   const cells = await rpc("layout",{mode:"grid",rect:{x:10,y:30,width:324,height:220}});
   if (cells.length !== 1 || cells[0].length !== 4) throw Error("bad native layout");
   const script = await rpc("pane.action",{id:pane.id,operation:"script",script:"6*7"});
   if (script !== "42") throw Error("bad pane script result");
   await rpc("pane.action",{id:pane.id,operation:"destroy"});
   const windows = await rpc("example.open",{example:"empty"});
   const empty = windows.windows.find(item => item.kind === "empty");
   if (!empty) throw Error("missing empty window");
   const attached = await rpc("window.attach",{id:empty.id});
   if (attached.windows.find(item => item.id === empty.id)?.webviewIds.length !== 1)
       throw Error("missing attached view");
   await rpc("window.close", {id:empty.id});
   await rpc("storage.save", {value:{host:"go",n:42}});
   const value = await rpc("storage.read");
   if (value?.host !== "go" || value?.n !== 42) throw Error("bad storage");
   window.__goSmoke = "PASS";
 })().catch(e => window.__goSmoke = "FAIL: " + String(e));
 return true;
})()`
	if _, err := resultOf(jade.ExecuteJavaScript(id, script)); err != nil {
		return err
	}
	for time.Now().Before(deadline) {
		result, err := resultOf(jade.ExecuteJavaScript(id, `window.__goSmoke || ""`))
		if err != nil {
			return err
		}
		var s string
		if json.Unmarshal([]byte(result), &s) == nil && s != "" {
			if s != "PASS" {
				return errors.New(s)
			}
			fmt.Println("PLAYGROUND_SMOKE_OK hello echo native-layout script attach storage")
			return h.smokeV3Examples()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("页面 IPC 冒烟验证超时")
}
