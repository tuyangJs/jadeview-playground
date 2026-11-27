package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

func (h *host) smokeExample(kind string, l launch, body string) error {
	h.mu.Lock()
	_, err := h.spawn(kind, l)
	id, view := uint32(0), uint32(0)
	if err == nil {
		for _, wid := range sorted(h.windows) {
			if w := h.windows[wid]; w.data["kind"] == kind {
				id, view = wid, w.controller
			}
		}
	}
	h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%s：%w", kind, err)
	}
	if id == 0 || view == 0 {
		return fmt.Errorf("%s：没有可测试的 WebView", kind)
	}
	defer jade.Close(id)
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		r, e := resultOf(jade.ExecuteJavaScript(view, "typeof window.jade?.invoke"))
		if e == nil && r == `"function"` {
			ready = true
			break
		}
		time.Sleep(75 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("%s：前端 IPC 未就绪", kind)
	}
	script := `(() => {
  window.__goV3Smoke = "";
  const rpc = async (action,args={}) => {
   let r = await window.jade.invoke("playground",{action,...args});
   if (typeof r==="string") r=JSON.parse(r);
   if (!r.ok) throw Error(r.error);
   return r.result;
  };
  (async () => {` + body + `})().then(
   value => window.__goV3Smoke = "OK "+String(value || ""),
   error => window.__goV3Smoke = "FAIL "+String(error)
  );
  return true;
 })()`
	if _, err = resultOf(jade.ExecuteJavaScript(view, script)); err != nil {
		return fmt.Errorf("%s：%w", kind, err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r, e := resultOf(jade.ExecuteJavaScript(view, `window.__goV3Smoke || ""`))
		if e != nil {
			return fmt.Errorf("%s：%w", kind, e)
		}
		var value string
		if json.Unmarshal([]byte(r), &value) == nil && value != "" {
			if !strings.HasPrefix(value, "OK ") {
				return fmt.Errorf("%s：%s", kind, value)
			}
			fmt.Printf("PLAYGROUND_V3_OK %s %s\n", kind, strings.TrimPrefix(value, "OK "))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s：交互验证超时", kind)
}

func (h *host) smokeV3Examples() error {
	if err := h.smokeExample("resources", launch{}, `
  const ready=await rpc("hello");
  const resource=await rpc("resource.open",{path:`+wire(h.asset())+`});
  if (resource.name!=="playground.png" || resource.bytes<=0 || !resource.capabilities)
   throw Error("bad resource metadata");
  if (!["native","controlled"].includes(resource.delivery.mode)) throw Error("bad delivery mode");
  const img=new Image();
  if (resource.delivery.mode==="controlled") img.crossOrigin="anonymous";
  await new Promise((resolve,reject)=>{
   img.onload=resolve;
   img.onerror=()=>reject(Error("failed to load preview "+resource.url));
   img.src=resource.url;
  });
  if (img.naturalWidth<=0) throw Error("preview has zero width");
  img.onload=img.onerror=null;
  img.removeAttribute("src");
  await rpc("resource.close",{url:resource.url});
  return resource.delivery.mode;
 `); err != nil {
		return err
	}
	permissionProbe := "true"
	if runtime.GOOS != "windows" {
		permissionProbe = "false"
	}
	if err := h.smokeExample("permissions", launch{}, `
  await rpc("permission",{value:1});
  await rpc("permission",{value:-1});
  if (!`+permissionProbe+`) {
   await rpc("permission",{value:0});
   return "three-state IPC";
  }
  if (!navigator.geolocation) throw Error("geolocation unavailable");
  await new Promise((resolve,reject)=>navigator.geolocation.getCurrentPosition(
   ()=>reject(Error("geolocation was allowed after deny")),
   error=>error.code===1 ? resolve() : reject(Error(error.message)),
   {timeout:7000}
  ));
  const events=await rpc("events",{after:0});
  if (!events.some(e=>e.name==="permission"&&e.data?.decision===-1))
   throw Error("native deny callback not observed");
  await rpc("permission",{value:0});
  return "deny callback";
 `); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return nil
	}
	duration := uint32(300)
	if err := h.smokeExample("splash", launch{SplashDuration: &duration}, `
  const hello=await rpc("hello");
  const w=hello.snapshot.windows.find(x=>x.kind==="splash");
  if (!w || w.splashDuration!==300 || w.style!=="default") throw Error("wrong splash configuration or title bar");
  for (let i=0; i<25; i++) {
   const state=await rpc("snapshot");
   if (state.windows.find(x=>x.kind==="splash")?.menuBarVisible===false)
    return "native title bar + menu hidden";
   await new Promise(resolve=>setTimeout(resolve,80));
  }
  throw Error("splash menu did not hide after frontend ready");
 `); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>JadeView popup smoke</title><p>local test</p>")
	})}
	defer server.Close()
	go server.Serve(listener)
	return h.smokeExample("webpopup", launch{}, `
  const popup = window.open(`+wire("http://"+listener.Addr().String()+"/")+`, "_blank", "popup,width=520,height=360");
  if (!popup) throw Error("window.open returned null");
  try {
   for (let i=0;i<40;i++){
    const events=await rpc("events",{after:0});
    if (events.some(e=>e.name==="webview-new-window" && e.data?.url?.startsWith("http://127.0.0.1:"))) return "native window.open";
    await new Promise(resolve=>setTimeout(resolve,100));
   }
   throw Error("native popup event not observed");
  } finally { popup.close(); }
 `)
}
