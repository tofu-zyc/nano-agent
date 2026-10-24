#!/usr/bin/env python3
"""网页界面检查：起一个一次性的网关（临时目录、假模型），用浏览器把拓扑图上的节点逐个点开。

检查：
- 每个配置项恰好出现在一个节点的面板里（不漏、不重复），面板全部能打开；
- 改数字、开关、文本、列表几种设置，回车或点别处就保存，刷新后还在；
- 以前页面的链接（设置、设备、定时任务、人格）跳到对应的节点；⌘K 搜设置能跳过去并高亮；
- 人格编辑器能保存；定时任务能在节点面板里新建、删除；
- 整个过程没有 JS 报错。

用法：make test-ui（先 make all）。需要 Python 的 playwright 和它的 chromium：
  pip install playwright && python3 -m playwright install chromium-headless-shell
截图放在 build/ui-check/。
"""
import glob
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
GATEWAY = os.path.join(ROOT, "gateway", "nano-gateway")
SHOTS = os.path.join(ROOT, "build", "ui-check")
PASSWORD = "ui-check-password"
FAILS = []


def ok(msg):
    print("  ok  ", msg)


def fail(msg):
    FAILS.append(msg)
    print("  FAIL", msg)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def start_gateway(tmp):
    port = free_port()
    home, nano = os.path.join(tmp, "home"), os.path.join(tmp, "nano")
    os.makedirs(home)
    os.makedirs(nano)
    sock = tempfile.mkdtemp(prefix="nui.", dir="/tmp")
    with open(os.path.join(nano, "gateway.toml"), "w") as f:
        f.write(f'[kernel]\nlisten = "unix:{sock}/k.sock"\ntoken = "ui-check"\n'
                f'[web]\nlisten = "127.0.0.1:{port}"\npassword = "{PASSWORD}"\n'
                '[cli]\nenabled = false\n[llm]\nprovider = "mock"\nmodel = "mock"\n')
    env = {"PATH": "/usr/bin:/bin", "HOME": home, "NANO_HOME": nano, "USERPROFILE": home, "XDG_CONFIG_HOME": os.path.join(home, ".config")}
    log = open(os.path.join(tmp, "gateway.log"), "w")
    proc = subprocess.Popen([GATEWAY, "-c", os.path.join(nano, "gateway.toml")], cwd=tmp, env=env,
                            stdout=subprocess.DEVNULL, stderr=log)
    url = f"http://127.0.0.1:{port}/"
    for _ in range(100):
        try:
            urllib.request.urlopen(url, timeout=1)
            return proc, url, nano, sock
        except Exception:
            time.sleep(0.1)
    proc.kill()
    raise SystemExit("网关没起来，看 " + os.path.join(tmp, "gateway.log"))


def launch(p):
    try:
        return p.chromium.launch()
    except Exception:
        shells = sorted(glob.glob(os.path.expanduser("~/.cache/ms-playwright/chromium_headless_shell-*/*/chrome-headless-shell")))
        if not shells:
            raise
        return p.chromium.launch(executable_path=shells[-1])


def main():
    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        raise SystemExit("缺少 playwright：pip install playwright && python3 -m playwright install chromium-headless-shell")
    if not os.access(GATEWAY, os.X_OK):
        raise SystemExit("先 make all")
    os.makedirs(SHOTS, exist_ok=True)
    tmp = tempfile.mkdtemp(prefix="nano-ui-")
    proc, url, nano, sock = start_gateway(tmp)
    errors = []
    try:
        with sync_playwright() as p:
            browser = launch(p)
            ctx = browser.new_context(viewport={"width": 1600, "height": 950}, color_scheme="dark")
            page = ctx.new_page()
            page.on("pageerror", lambda e: errors.append("pageerror: " + str(e)))
            # 网络请求的 4xx 是预期内的（比如故意填错的值、还没写的人格文件），只看 5xx 和代码里的报错
            page.on("console", lambda m: m.type == "error" and not m.text.startswith("Failed to load resource") and errors.append("console: " + m.text))
            page.on("response", lambda r: r.status >= 500 and errors.append(f"HTTP {r.status} {r.request.method} {r.url}"))
            run(page, url, nano)
            browser.close()
    finally:
        proc.terminate()
        try:
            proc.wait(5)
        except subprocess.TimeoutExpired:
            proc.kill()
        shutil.rmtree(tmp, ignore_errors=True)
        shutil.rmtree(sock, ignore_errors=True)
    if errors:
        fail("页面上有 JS 报错：\n    " + "\n    ".join(errors[:10]))
    else:
        ok("整个过程没有 JS 报错")
    print(f"ui: {len(FAILS)} failures（截图在 {os.path.relpath(SHOTS, ROOT)}/）")
    sys.exit(1 if FAILS else 0)


def api(page, method, path, body=None):
    js = """async ([m, p, b]) => {
      const r = await fetch(p, { method: m, headers: { 'X-Nano': '1', 'Content-Type': 'application/json' }, body: b === null ? undefined : JSON.stringify(b) });
      return { status: r.status, body: await r.text() };
    }"""
    r = page.evaluate(js, [method, path, body])
    try:
        return r["status"], json.loads(r["body"])
    except ValueError:
        return r["status"], r["body"]


def fields(page):
    return {f["key"]: f for f in api(page, "GET", "/api/config")[1]["fields"]}


def open_node(page, node_id):
    if page.locator(".insp").count():  # 先关掉面板：加宽的面板会盖住图右边的节点
        page.click(".insp header button[aria-label='关闭']")
        page.wait_for_timeout(250)
    page.click(f'g.node[data-id="{node_id}"]')
    page.wait_for_selector(".insp")
    page.wait_for_timeout(350)
    # 展开所有“更多设置”
    for b in page.locator(".insp button.more").all():
        if "更多设置" in b.inner_text():
            b.click()
    page.wait_for_timeout(150)
    return page.locator(".insp header h3").inner_text()


def panel_keys(page):
    ids = page.eval_on_selector_all(".insp [id^='f-']", "els => els.map(e => e.id.slice(2))")
    return ids


def run(page, url, nano):
    page.goto(url)
    page.fill("input[type=password]", PASSWORD)
    page.keyboard.press("Enter")
    page.wait_for_selector("g.node.gateway", timeout=15000)
    page.wait_for_timeout(800)
    page.screenshot(path=os.path.join(SHOTS, "topology.png"))

    # ---- 左边只剩拓扑和会话 ----
    rail = page.locator("nav.rail a.item").all_inner_texts()
    if [t.strip() for t in rail] == ["拓扑", "会话"]:
        ok("左边只有拓扑和会话两个页面")
    else:
        fail(f"左边的页面：{rail}")

    # ---- 每个配置项恰好在一个节点里 ----
    all_fields = fields(page)
    seen = {}
    nodes = page.eval_on_selector_all("g.node", "els => els.map(e => e.dataset.id)")
    for nid in nodes:
        if nid == "more":
            continue
        title = open_node(page, nid)
        page.screenshot(path=os.path.join(SHOTS, "node-" + nid.replace(":", "_").replace("/", "_") + ".png"))
        for k in panel_keys(page):
            seen.setdefault(k, []).append(f"{nid}（{title}）")
    missing = [k for k in all_fields if k not in seen]
    dup = {k: v for k, v in seen.items() if len(set(v)) > 1}
    unknown = [k for k in seen if k not in all_fields]
    if missing:
        fail("这些配置项哪个节点里都没有：" + "、".join(missing))
    if dup:
        fail("这些配置项出现在好几个节点里：" + json.dumps(dup, ensure_ascii=False))
    if unknown:
        fail("面板里有网关不认识的配置项：" + "、".join(unknown))
    if not (missing or dup or unknown):
        ok(f"{len(all_fields)} 个配置项都恰好在一个节点里（点开了 {len(nodes)} 个节点）")

    # ---- 改设置：数字、文本、开关、列表，刷新后还在 ----
    open_node(page, "agent")
    box = page.locator("#f-agent\\.max_steps input")
    box.fill("0")
    box.press("Enter")
    page.wait_for_timeout(500)
    box = page.locator("#f-agent\\.run_timeout input")
    box.fill("2h")
    box.press("Tab")
    page.wait_for_timeout(500)
    page.locator("#f-session\\.timestamps button.switch").click()
    page.wait_for_timeout(500)
    chip = page.locator("#f-access\\.admins input")
    chip.fill("tg:42")
    chip.press("Enter")
    page.wait_for_timeout(600)
    open_node(page, "memory")
    box = page.locator("#f-context\\.compact_at input")
    box.fill("0.8")
    box.press("Enter")
    page.wait_for_timeout(500)
    page.reload()
    page.wait_for_selector("g.node.gateway")
    page.wait_for_timeout(600)
    f = fields(page)
    got = (f["agent.max_steps"]["value"], f["agent.run_timeout"]["value"], f["session.timestamps"]["value"], f["access.admins"]["value"], f["context.compact_at"]["value"])
    if got == (0, "2h", False, ["tg:42"], 0.8):
        ok("数字、文本、开关、列表改了自动保存，刷新后还在")
    else:
        fail(f"改设置没保存：{got}")
    open_node(page, "agent")
    v = page.locator("#f-agent\\.max_steps input").input_value()
    if v == "0":
        ok("刷新后面板里显示的是改过的值")
    else:
        fail(f"刷新后面板里的值：{v}")
    # 改了不合法的值：报错，不保存
    box = page.locator("#f-agent\\.run_timeout input")
    box.fill("一会儿")
    box.press("Enter")
    page.wait_for_timeout(600)
    if fields(page)["agent.run_timeout"]["value"] == "2h":
        ok("不合法的值不会存进去")
    else:
        fail("不合法的时限被存进去了")
    page.keyboard.press("Escape")
    page.wait_for_timeout(200)

    # ---- 网关节点：改过的设置一览 ----
    open_node(page, "gw")
    text = page.locator(".insp").inner_text()
    if "网页里改过的设置" in text and "工具调用轮数上限" in text:
        ok("网关节点列出网页里改过的设置")
    else:
        fail("网关节点没列出改过的设置")
    page.locator(".insp .p-srow", has_text="工具调用轮数上限").click()
    page.wait_for_timeout(500)
    if page.locator(".insp header h3").inner_text() == "Agent" and page.locator("#f-agent\\.max_steps.hl").count():
        ok("点改过的设置跳到它所在的节点并高亮")
    else:
        fail("点改过的设置没跳对")

    # ---- 以前页面的链接 ----
    for hash_, want in [("#/settings?focus=context.keep_turns", "上下文与记忆"), ("#/devices", "接入机器"), ("#/schedules", "定时任务"), ("#/settings", "网关")]:
        page.goto(url + hash_)
        page.wait_for_timeout(700)
        h = page.locator(".insp header h3").inner_text() if page.locator(".insp").count() else "（没有面板）"
        if h == want and page.url.endswith("#/"):
            ok(f"{hash_} → {want} 节点")
        else:
            fail(f"{hash_} 打开的是 {h}，地址 {page.url}")
    page.goto(url + "#/personas")
    page.wait_for_timeout(700)
    if page.locator(".sheet[aria-label='编辑人格']").count():
        ok("#/personas → 人格编辑器")
        page.fill(".sheet textarea", "# 测试人格\n你是界面检查用的人格。")
        page.click(".sheet button.primary")
        page.wait_for_timeout(600)
        persona = os.path.join(nano, "personas", "default.md")
        if os.path.exists(persona) and "界面检查" in open(persona, encoding="utf-8").read():
            ok("人格编辑器保存到 default.md")
        else:
            fail("人格没保存")
        page.keyboard.press("Escape")
        page.wait_for_timeout(300)
        if page.locator(".sheet[aria-label='编辑人格']").count():
            fail("Esc 没关掉人格编辑器")
    else:
        fail("#/personas 没打开人格编辑器")

    # ---- ⌘K 搜设置 ----
    page.keyboard.press("Escape")
    page.wait_for_timeout(200)  # Esc 在下一轮事件里才关面板（先让弹窗处理），别和下一个按键挤在一起
    page.keyboard.press("Control+k")
    page.wait_for_selector(".pal input")
    page.fill(".pal input", "空闲整理间隔")
    page.wait_for_timeout(200)
    page.keyboard.press("Enter")
    page.wait_for_timeout(600)
    if page.locator(".insp header h3").inner_text() == "上下文与记忆" and page.locator("#f-context\\.idle_compact.hl").count():
        ok("⌘K 搜设置，回车打开它所在的节点并高亮")
    else:
        page.screenshot(path=os.path.join(SHOTS, "palette-fail.png"))
        h = page.locator(".insp header h3").inner_text() if page.locator(".insp").count() else "（没有面板）"
        fail(f"⌘K 搜设置没跳对：面板 {h}，路由 {page.url}")

    # 用户自己的说法也要搜得到
    page.keyboard.press("Escape")
    page.wait_for_timeout(200)
    page.keyboard.press("Control+k")
    page.wait_for_selector(".pal input")
    page.fill(".pal input", "工具调用上限")
    page.wait_for_timeout(200)
    page.keyboard.press("Enter")
    page.wait_for_timeout(600)
    if page.locator("#f-agent\\.max_steps.hl").count():
        ok("搜“工具调用上限”找到 Agent 节点里的工具调用轮数上限")
    else:
        fail("搜“工具调用上限”没找到对应的设置")

    # ---- 定时任务：在节点面板里新建、删除 ----
    open_node(page, "sched")
    page.click(".insp button:has-text('新建任务')")
    page.wait_for_selector("form.sheet")
    page.fill("form.sheet input[placeholder='早报']", "界面检查任务")
    page.fill("form.sheet textarea", "说一句话")
    page.click("form.sheet button.primary")
    page.wait_for_timeout(800)
    tasks = api(page, "GET", "/api/schedules")[1].get("tasks") or []
    t = [x for x in tasks if x["name"] == "界面检查任务"]
    if t and page.locator(".insp .task", has_text="界面检查任务").count():
        ok("定时任务节点里新建任务")
        page.on("dialog", lambda d: d.accept())
        page.locator(".insp .task", has_text="界面检查任务").locator("button[title='删除']").click()
        page.wait_for_timeout(800)
        if not [x for x in (api(page, "GET", "/api/schedules")[1].get("tasks") or []) if x["name"] == "界面检查任务"]:
            ok("定时任务节点里删除任务")
        else:
            fail("任务没删掉")
    else:
        fail(f"新建任务没成功：{tasks}")

    # ---- 网关换了版本：开着的页面自己刷新成新版本（不然新加的设置在旧页面上找不到） ----
    def bump(route):
        resp = route.fetch()
        data = resp.json()
        data.setdefault("runtime", {})["version"] = "9.9.9"
        route.fulfill(response=resp, json=data)
    page.route("**/api/overview", bump)
    with page.expect_navigation(timeout=8000):
        page.evaluate("window.__nano.refresh()")
    page.wait_for_selector("g.node.gateway")
    page.wait_for_timeout(500)
    if page.locator(".toast", has_text="页面已刷新成新版本").count() or "页面已刷新成新版本" in page.locator("body").inner_text():
        ok("网关换了版本，开着的页面自己刷新并提示")
    else:
        fail("网关换了版本以后页面没有提示")
    navs = []
    page.on("framenavigated", lambda f: f == page.main_frame and navs.append(f.url))
    page.evaluate("window.__nano.refresh()")
    page.wait_for_timeout(800)
    if not navs:
        ok("刷新以后版本没再变，就不再刷新")
    else:
        fail(f"版本没变也刷新了页面：{navs}")
    page.unroute("**/api/overview")
    page.reload()  # 换回真实的版本号
    page.wait_for_selector("g.node.gateway")

    # ---- 窄一点的窗口也能用 ----
    page.set_viewport_size({"width": 1280, "height": 760})
    open_node(page, "add:machine")
    page.screenshot(path=os.path.join(SHOTS, "narrow-machines.png"))
    if "别的电脑" in page.locator(".insp").inner_text():
        ok("接入机器面板（1280 宽）")
    else:
        fail("接入机器面板内容不对")


if __name__ == "__main__":
    main()
