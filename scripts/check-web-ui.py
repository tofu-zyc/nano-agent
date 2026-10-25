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
    # 节点有 0.45 秒的淡入：等它们真的画出来再截图，免得截到空白
    page.wait_for_function(
        "() => { const n = document.querySelector('g.node.agent'); return n && getComputedStyle(n).opacity === '1'; }",
        timeout=8000,
    )
    page.wait_for_timeout(400)
    page.screenshot(path=os.path.join(SHOTS, "topology.png"))

    # ---- 左边只剩拓扑和会话 ----
    rail = page.locator("nav.rail a.item").all_inner_texts()
    if [t.strip() for t in rail] == ["拓扑", "会话"]:
        ok("左边只有拓扑和会话两个页面")
    else:
        fail(f"左边的页面：{rail}")

    # ---- 同一个东西在图上只有一个节点（比如内置 Telegram 不能出现两次） ----
    titles = page.eval_on_selector_all("g.node", "els => els.map(e => e.dataset.id + '|' + (e.querySelector('.t1')?.textContent ?? ''))")
    seen_t = {}
    for t in titles:
        nid, title = t.split("|", 1)
        if nid.startswith("c:"):
            seen_t.setdefault(title.strip(), []).append(nid)
    dups = {k: v for k, v in seen_t.items() if len(v) > 1}
    if dups:
        fail(f"入口列里有重复的节点：{dups}")
    else:
        ok("入口列里每个渠道、机器人只有一个节点")

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

    # ---- v0.5 的节点：每个 agent、Agent 默认值、机器人 ----
    agents = api(page, "GET", "/api/agents")[1]["items"]
    bots = api(page, "GET", "/api/bots")[1]["items"]
    node_ids = page.eval_on_selector_all("g.node", "els => els.map(e => e.dataset.id)")
    for aid in [a["id"] for a in agents]:
        if "ag:" + aid not in node_ids:
            fail(f"agent {aid} 在图上没有节点")
    if "agdefault" not in node_ids or "add:agent" not in node_ids or "add:bot" not in node_ids:
        fail("图上缺 Agent 默认值 / 添加 agent / 添加机器人 节点")
    else:
        ok(f"{len(agents)} 个 agent 各有节点，另有 Agent 默认值、添加 agent、添加机器人")
    title = open_node(page, "ag:default")
    text = page.locator(".insp").inner_text()
    if "人格" in text and "机器人" in text:
        ok("agent 节点里有它自己的人格和绑定的机器人")
    else:
        fail("agent 节点里看不到人格或机器人：" + text[:200])
    # 编辑这个 agent 的人格：自带的两个（小纳、大肥鱼）都要能看见、能点
    page.locator(".insp button:has-text('编辑人格')").first.click()
    page.wait_for_selector(".sheet[aria-label='编辑人格']")
    page.wait_for_timeout(400)
    page.screenshot(path=os.path.join(SHOTS, "agent-persona.png"))
    sheet = page.locator(".sheet[aria-label='编辑人格']").inner_text()
    if "小纳" in sheet and "大肥鱼" in sheet:
        ok("编辑 agent 的人格时能看见自带的两个人格（小纳、大肥鱼）")
    else:
        fail("编辑 agent 的人格时看不到自带的两个人格：" + sheet[:300])
    # 自带的两个都能点：点「大肥鱼」编辑框会换成它
    page.locator(".sheet[aria-label='编辑人格'] button.preset:has-text('大肥鱼')").first.click()
    page.wait_for_timeout(300)
    body = page.locator(".sheet[aria-label='编辑人格'] textarea").input_value()
    if "大肥鱼" in body:
        ok("点自带的「大肥鱼」会把人格填进编辑框")
    else:
        fail("点自带人格没有填进编辑框：" + body[:120])
    page.screenshot(path=os.path.join(SHOTS, "agent-persona.png"))
    page.keyboard.press("Escape")
    page.wait_for_timeout(300)
    if "群里插话积极度" in text:
        ok("agent 节点里有群聊行为（插话积极度等）")
        if "少插话" in text or "不主动说" in text:
            ok("群聊积极度的选项是中文（不主动说 / 少插话 / 正常 / 多插话）")
        else:
            fail("群聊积极度还是英文选项：" + text[:300])
    else:
        fail("agent 节点里没有群聊设置：" + text[:200])
    # 在 agent 节点里改一条覆盖设置：存进去的是它自己的值
    first = page.locator(".insp .setting input, .insp .setting select").first
    if first.count():
        first.fill("42") if first.evaluate("e => e.tagName === 'INPUT'") else None
        first.press("Enter")
        page.wait_for_timeout(700)
        got = api(page, "GET", "/api/agents")[1]["items"]
        mine = [a for a in got if a["id"] == "default"][0]
        vals = {s["key"]: s["value"] for s in mine["settings"]}
        if any(v is not None for v in vals.values()):
            ok("agent 自己的设置能保存（覆盖全局）")
        else:
            fail("agent 的设置没存上：" + json.dumps(vals, ensure_ascii=False))
        api(page, "PATCH", "/api/agents/default", {"settings": {k: None for k in vals}})
    # 机器人节点
    if bots:
        open_node(page, "c:telegram")
        text = page.locator(".insp").inner_text()
        if "绑定的 agent" in text:
            ok("Telegram 节点里能改它绑的 agent")
        else:
            fail("Telegram 节点里没有绑定 agent 的地方")

    check_living(page, url, nano)
    check_bots(page)

    # ---- 新建 agent 时直接挑一个自带人格：人格文件里就是它 ----
    open_node(page, "add:agent")
    page.locator(".insp input").first.fill("测试人格")
    page.locator(".insp select").first.select_option(index=2)  # 自己写 / 小纳 / 大肥鱼
    page.locator(".insp button.style:has-text('拟人型')").first.click()  # 建的时候就能选类型
    page.locator(".insp button:has-text('创建')").first.click()
    page.wait_for_timeout(1200)
    made = [a for a in api(page, "GET", "/api/agents")[1]["items"] if a["name"] == "测试人格"]
    if not made:
        fail("新建 agent 没成功")
    else:
        made = made[0]
        pfile = os.path.join(nano, "agents", made["id"], "persona.md")
        if made.get("style") == "life" and made.get("life", {}).get("next"):
            ok("新建 agent 时能直接选拟人型（建好就定下了第一次醒来的时间）")
        else:
            fail("新建时选的拟人型没生效：" + json.dumps({k: made.get(k) for k in ("style", "life")}, ensure_ascii=False))
        if os.path.exists(pfile) and "大肥鱼" in open(pfile, encoding="utf-8").read():
            ok("新建 agent 时能直接选自带人格（大肥鱼 写进了它的人格文件）")
        else:
            fail("新建 agent 的人格文件不对：" + (open(pfile, encoding="utf-8").read()[:120] if os.path.exists(pfile) else "文件不存在"))
        api(page, "DELETE", "/api/agents/" + made["id"])

    # ---- 群和私聊：3 个 agent、2 个群、1 个私聊、1 个停用的机器人 ----
    api(page, "POST", "/api/agents", {"name": "小鲍", "emoji": "🦊"})
    api(page, "POST", "/api/agents", {"name": "阿强", "emoji": "🐻"})
    agents = api(page, "GET", "/api/agents")[1]["items"]
    byname = {a["name"]: a["id"] for a in agents}
    if "小鲍" not in byname or "阿强" not in byname:
        fail("中文名字的 agent 没建成：" + json.dumps(byname, ensure_ascii=False))
        bao = qiang = "default"
    elif byname["小鲍"] in ("agent", "agent-2") or byname["阿强"] in ("agent", "agent-2"):
        fail("中文名字的 ID 还是 agent / agent-2：" + json.dumps(byname, ensure_ascii=False))
        bao, qiang = byname["小鲍"], byname["阿强"]
    else:
        ok("中文名字的 agent 用短随机 ID（小鲍 → " + byname["小鲍"] + "）")
        bao, qiang = byname["小鲍"], byname["阿强"]
    code, room = api(page, "POST", "/api/rooms", {"name": "产品组", "members": ["default", bao, qiang], "mode": "mention"})
    code2, room2 = api(page, "POST", "/api/rooms", {"name": "值班", "members": [bao, qiang]})
    code3, dm = api(page, "POST", "/api/rooms", {"members": ["default", bao], "dm": True})
    code4, _ = api(page, "POST", "/api/bots", {"kind": "telegram", "name": "旧号", "token": "123456:abcDEF", "agent": bao, "enabled": False})
    if not (code == 200 and code2 == 200 and code3 == 200 and code4 == 200):
        fail("建群 / 私聊 / 机器人接口失败：" + str([code, code2, code3, code4]))
    bot = [b for b in api(page, "GET", "/api/bots")[1]["items"] if b["name"] == "旧号"][0] if code4 == 200 else None
    page.reload()
    page.wait_for_selector("g.node.gateway")
    page.wait_for_function(
        "() => { const n = document.querySelector('g.node.agent'); return n && getComputedStyle(n).opacity === '1'; }",
        timeout=8000,
    )
    page.wait_for_timeout(500)
    ids = page.eval_on_selector_all("g.node", "els => els.map(e => e.dataset.id)")
    want = ["rm:" + room["id"], "rm:" + room2["id"], "dm:" + dm["id"], "add:room"]
    if bot:
        want.append("c:" + bot["channel"])
    missing = [w for w in want if w not in ids]
    if missing:
        fail("图上缺群 / 私聊 / 停用机器人的节点：" + json.dumps(missing, ensure_ascii=False))
    else:
        ok("2 个群、1 个私聊、1 个停用的机器人都在图上")
    # 群单独一列：群节点的 x 在 agent 左边（连线不会反向绕）
    xof = page.evaluate("""() => {
      const out = {};
      for (const g of document.querySelectorAll('g.node')) {
        const m = g.transform.baseVal.consolidate();
        out[g.dataset.id] = m ? m.matrix.e : null;
      }
      return out;
    }""")
    room_x = xof.get("rm:" + room["id"])
    agent_xs = [x for k, x in xof.items() if k.startswith("ag:") and x is not None]
    if room_x is not None and agent_xs and room_x + 200 <= min(agent_xs):
        ok("群单独一列，在 Agent 左边")
    else:
        fail(f"群和 agent 挤在一起：群 x={room_x}，agent x={agent_xs}")
    # 连线：群 → 成员、私聊 → 两个人、停用的机器人 → 它绑的 agent、群会话 → 群节点
    edges = page.eval_on_selector_all("path.edge", "els => els.map(e => e.dataset.id)")
    checks = {
        "群 → 小鲍": "rm:" + room["id"] + ">ag:" + bao,
        "群 → 阿强": "rm:" + room["id"] + ">ag:" + qiang,
        "私聊 → 小纳": "dm:" + dm["id"] + ">ag:default",
        "私聊 → 小鲍": "dm:" + dm["id"] + ">ag:" + bao,
        "群会话 → 群节点": "s:" + room["sid"] + ">rm:" + room["id"],
    }
    if bot:
        checks["停用的机器人 → 它绑的 agent"] = "c:" + bot["channel"] + ">ag:" + bao
    bad = [k for k, v in checks.items() if v not in edges]
    if bad:
        fail("这些连线不对：" + "、".join(bad) + " / " + json.dumps(edges, ensure_ascii=False)[:500])
    else:
        ok("群 / 私聊 / 机器人 / 群会话的连线都连对了")
    if ("s:" + room["sid"] + ">ag:default") in edges:
        fail("群的会话还连到了默认 agent")
    # 入口直接连 agent 的虚线：和原来一样是直连，线画在卡片下面（被挡住的段落看不见）
    page.screenshot(path=os.path.join(SHOTS, "topology-groups.png"))
    # 群面板
    open_node(page, "rm:" + room["id"])
    page.screenshot(path=os.path.join(SHOTS, "room.png"))
    text = page.locator(".insp").inner_text()
    if "成员" in text and "怎么说话" in text and "每天最多用多少 token" in text:
        ok("群面板能改成员、说话方式和预算")
    else:
        fail("群面板缺东西：" + text[:200])
    sessions = {x["sid"]: x for x in api(page, "GET", "/api/overview")[1]["sessions"]}
    if sessions.get(room["sid"], {}).get("title") == "产品组":
        ok("群的会话在列表里显示群名")
    else:
        fail("群的会话标题不对：" + json.dumps(sessions.get(room["sid"], {}).get("title"), ensure_ascii=False))
    if any("/room:" in sid for sid in sessions):
        fail("agent 在群里旁听的会话不应该出现在会话列表里")
    # 暂停一下，看看面板和接口对得上
    api(page, "PATCH", "/api/rooms/" + room["id"], {"paused": True})
    page.reload()
    page.wait_for_selector("g.node.gateway")
    page.wait_for_timeout(600)
    paused = [r for r in api(page, "GET", "/api/rooms")[1]["items"] if r["id"] == room["id"]]
    if paused and paused[0]["paused"] is True:
        ok("群能暂停")
    else:
        fail("暂停没生效")
    # 收拾干净，后面的检查不受影响
    for r in (room, room2, dm):
        if r.get("id"):
            api(page, "DELETE", "/api/rooms/" + r["id"])
    if bot:
        api(page, "DELETE", "/api/bots/" + bot["id"])
    for aid in (bao, qiang):
        if aid != "default":
            api(page, "DELETE", "/api/agents/" + aid)

    # ---- 会话面板：新建一个只给这个会话用的人格文件 ----
    page.reload()
    page.wait_for_selector("g.node.gateway")
    page.wait_for_timeout(800)
    sessions = page.eval_on_selector_all("g.node.session", "els => els.map(e => e.dataset.id)")
    if sessions:
        open_node(page, sessions[0])
        insp = page.locator(".insp").inner_text()
        if "人格" in insp:
            page.locator(".insp button:has-text('新建一个只给这个会话用的人格')").first.click()
            page.wait_for_selector(".sheet[aria-label='编辑人格']")
            page.wait_for_timeout(400)
            sheet = page.locator(".sheet[aria-label='编辑人格']").inner_text()
            if "人格文件" in sheet and "小纳" in sheet and "大肥鱼" in sheet:
                ok("会话里能新建人格文件，并且能套用自带的两个")
            else:
                fail("会话里新建人格的界面不对：" + sheet[:300])
            page.screenshot(path=os.path.join(SHOTS, "session-persona.png"))
            page.keyboard.press("Escape")
        else:
            fail("会话面板里没有「人格」：" + insp[:200])
    else:
        fail("图上没有会话节点")

    # ---- 改设置：数字、文本、开关、列表，刷新后还在 ----
    open_node(page, "agdefault")
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
    open_node(page, "agdefault")
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
    if page.locator(".insp header h3").inner_text() == "Agent 默认值" and page.locator("#f-agent\\.max_steps.hl").count():
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
        # 人格现在是每个 agent 自己的：默认 agent 的文件在 agents/default/persona.md
        persona = os.path.join(nano, "agents", "default", "persona.md")
        if os.path.exists(persona) and "界面检查" in open(persona, encoding="utf-8").read():
            ok("人格编辑器保存到 agents/default/persona.md")
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


def check_bots(page):
    """v0.7：QQ、微信机器人。扫码的接口在浏览器里拦下来（不碰腾讯的真服务），停用的机器人也不会去连。"""
    open_node(page, "add:bot")
    page.locator(".insp select.p-select").first.select_option("qq")
    page.wait_for_timeout(200)
    text = page.locator(".insp").inner_text()
    if "扫码创建机器人" in text and "生成二维码" in text and "手动填" in text and "Bot Token" not in text:
        ok("添加机器人：选 QQ 是扫码创建，也能手动填")
    else:
        fail("选 QQ 以后面板不对：" + text[:400])
    svg = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 4 4"><rect width="4" height="4" fill="#fff"/></svg>'
    seen = []
    def reg(route):
        seen.append((route.request.method, route.request.post_data or ""))
        route.fulfill(json={"url": "https://q.qq.com/qqbot/openclaw/connect.html?task_id=t", "qr_svg": svg} if route.request.method == "POST" else {"ok": True})
    page.route("**/api/bots/register", reg)
    page.locator(".insp button:has-text('生成二维码')").click()
    page.wait_for_selector(".insp .qrbox .qr svg")
    text = page.locator(".insp").inner_text()
    page.screenshot(path=os.path.join(SHOTS, "bot-qq-qr.png"))
    body = json.loads(seen[0][1]) if seen else {}
    if "QQ扫一扫" in text.replace(" ", ""):
        if body.get("kind") == "qq" and body.get("agent") == "default":
            ok("QQ 扫码：显示二维码，请求带着种类和绑给谁")
        else:
            fail(f"扫码请求不对：{body}")
    else:
        fail("QQ 二维码没显示：" + text[:300])
    page.locator(".insp button:has-text('取消')").click()
    page.wait_for_timeout(200)
    page.locator(".insp button:has-text('手动填')").click()
    page.wait_for_timeout(150)
    text = page.locator(".insp").inner_text()
    if "App ID" in text and "QQ 开放平台" in text and "改成扫码" in text:
        ok("QQ 手动填 AppID / AppSecret")
    else:
        fail("QQ 手动填不对：" + text[:300])
    page.locator(".insp select.p-select").first.select_option("weixin")
    page.wait_for_timeout(200)
    text = page.locator(".insp").inner_text()
    if "扫码连接微信" in text and "不能拉群" in text and "App ID" not in text and "添加" not in [b.inner_text() for b in page.locator(".insp .p-actions button").all()]:
        ok("添加机器人：选微信只能扫码连接，说明了只能自己聊、不能拉群")
    else:
        fail("选微信以后面板不对：" + text[:400])
    page.unroute("**/api/bots/register")

    # 停用的微信机器人：面板里能重新扫码，没有“保存凭据”
    st, wx = api(page, "POST", "/api/bots", {"kind": "weixin", "name": "我的微信", "agent": "default", "token": "x", "enabled": False})
    st2, qq = api(page, "POST", "/api/bots", {"kind": "qq", "name": "我的 QQ", "agent": "default", "app_id": "1", "app_secret": "y", "enabled": False})
    if st != 200 or st2 != 200:
        fail(f"加机器人失败：{st} {wx} {st2} {qq}")
        return
    page.evaluate("window.__nano.refresh()")
    page.wait_for_timeout(500)
    open_node(page, "add:bot")
    page.locator(".insp .p-srow:has-text('我的微信')").click()
    page.wait_for_timeout(300)
    text = page.locator(".insp").inner_text()
    page.screenshot(path=os.path.join(SHOTS, "bot-weixin.png"))
    if "微信" in text and "重新扫码" in text and "保存凭据" not in text and wx.get("channel") == "wx.weixin":
        ok("微信机器人面板：种类是微信，能重新扫码")
    else:
        fail("微信机器人面板不对：" + json.dumps(wx, ensure_ascii=False) + " " + text[:400])
    open_node(page, "add:bot")
    page.locator(".insp .p-srow:has-text('我的 QQ')").click()
    page.wait_for_timeout(300)
    text = page.locator(".insp").inner_text()
    if "QQ" in text and "App ID" in text and "保存凭据" in text and qq.get("channel") == "qq.qq":
        ok("QQ 机器人面板：种类是 QQ，能改 AppID / AppSecret")
    else:
        fail("QQ 机器人面板不对：" + json.dumps(qq, ensure_ascii=False) + " " + text[:400])
    for b in (wx, qq):
        api(page, "DELETE", "/api/bots/" + b["id"])


def check_living(page, url, nano):
    """v0.6：两种 agent（工具型 / 拟人型）、拟人型的状态和设置、记忆节点、内心会话只读、记忆说明折叠显示。"""
    # 默认是工具型：拟人那组设置不显示
    open_node(page, "ag:default")
    page.wait_for_selector(".insp .styles")
    text = page.locator(".insp").inner_text()
    if "工具型" in text and "拟人型" in text and "主动找谁聊天" not in text and "长期记忆" in text:
        ok("agent 节点：有工具型 / 拟人型可选，工具型不显示拟人的设置，有长期记忆")
    else:
        fail("agent 节点的类型和记忆不对：" + text[:400])
    # 改成拟人型：状态卡、拟人的设置都出来
    page.locator(".insp button.style:has-text('拟人型')").first.click()
    page.wait_for_timeout(900)
    me = [a for a in api(page, "GET", "/api/agents")[1]["items"] if a["id"] == "default"][0]
    text = page.locator(".insp").inner_text()
    page.screenshot(path=os.path.join(SHOTS, "agent-life.png"))
    if me.get("style") == "life" and "现在叫醒它" in text and "醒着的时间" in text and "主动找谁聊天" in text:
        ok("点「拟人型」：存成拟人型，面板里有状态卡（叫醒它）和拟人的设置")
    else:
        fail("改成拟人型以后面板不对：" + json.dumps(me.get("style")) + " " + text[:400])
    card = page.eval_on_selector('g.node[data-id="ag:default"]', "e => e.textContent")
    if "拟人" in card:
        ok("拓扑图的 agent 卡片上标了「拟人」和它的状态")
    else:
        fail("agent 卡片上没标拟人：" + card)
    # 记忆：写一个人、一条事、自我认知，记忆节点里能看能搜能改
    api(page, "PUT", "/api/agents/default/memory/item?path=", {"raw": "---\ntitle: 张三\ntype: 人物\nsummary: 产品经理，养了两只猫\n---\n说话直。"})
    api(page, "PUT", "/api/agents/default/memory/item?path=", {"raw": "---\ntitle: 国庆去成都\ntype: 事件\n---\n吃了三次火锅。"})
    api(page, "PUT", "/api/agents/default/memory/self", {"text": "我叫小纳，喜欢看星星。"})
    open_node(page, "memory")
    page.wait_for_timeout(500)
    text = page.locator(".insp").inner_text()
    if all(k in text for k in ["人物", "记忆", "自我认知", "梦境日志", "张三", "产品经理"]):
        ok("记忆节点：人物、记忆、自我认知、梦境日志四栏，能看到记下的人")
    else:
        fail("记忆节点不对：" + text[:400])
    page.locator(".insp .p-seg button:has-text('记忆')").first.click()
    page.wait_for_timeout(300)
    if "国庆去成都" in page.locator(".insp").inner_text():
        ok("记忆栏里有记下的事")
    else:
        fail("记忆栏里没有记下的事")
    page.locator(".insp input[placeholder^='搜记忆']").fill("火锅")
    page.wait_for_timeout(700)
    if "国庆去成都" in page.locator(".insp .items").inner_text():
        ok("记忆能按关键词搜（火锅 → 国庆去成都）")
    else:
        fail("搜不到记忆")
    page.locator(".insp .item button.head").first.click()
    page.wait_for_timeout(400)
    raw = page.locator(".insp .item textarea").first.input_value()
    page.locator(".insp .item textarea").first.fill(raw.replace("三次", "五次"))
    page.locator(".insp .item button:has-text('保存')").first.click()
    page.wait_for_timeout(600)
    page.screenshot(path=os.path.join(SHOTS, "memory.png"))
    st, got = api(page, "GET", "/api/agents/default/memory/item?path=" + urllib.request.quote("国庆去成都.md"))
    if st == 200 and "五次" in got.get("raw", ""):
        ok("记忆节点里改一条记忆能保存")
    else:
        fail(f"改记忆没存上：{st} {got}")
    page.locator(".insp .p-seg button:has-text('自我认知')").first.click()
    page.wait_for_timeout(300)
    if "看星星" in page.locator(".insp textarea").first.input_value():
        ok("自我认知能看到")
    else:
        fail("自我认知没显示")
    # 会话里：网关放进消息前面的记忆说明折叠显示，不把 [记忆] 原样摊出来
    sess = os.path.join(nano, "data", "sessions")
    os.makedirs(sess, exist_ok=True)
    with open(os.path.join(sess, "memtest.jsonl"), "w", encoding="utf-8") as f:
        f.write(json.dumps({"role": "user", "ts": 1792800000, "content": "[系统] 这个会话没有挂载工作目录\n[记忆]\n这是你的长期记忆\n## 对方：主人\n喜欢手冲\n[/记忆]\n[近况]\n- 10-24 14:00 看了篇讲猫的文章\n[/近况]\n[2026-10-24 周五 15:08 · 网页] 你好呀"}, ensure_ascii=False) + "\n")
        f.write(json.dumps({"role": "assistant", "ts": 1792800001, "content": "你好"}, ensure_ascii=False) + "\n")
    with open(os.path.join(sess, "@default%2Flife.jsonl"), "w", encoding="utf-8") as f:  # 和网关的 url.PathEscape 一样
        f.write(json.dumps({"role": "user", "ts": 1792800000, "content": "[系统] 现在是 10-24 周五 15:20。你醒着，手头没事。"}, ensure_ascii=False) + "\n")
        f.write(json.dumps({"role": "assistant", "ts": 1792800001, "content": "看了会儿星星的资料，心情不错。"}, ensure_ascii=False) + "\n")
    page.goto(url + "#/sessions/memtest")
    page.wait_for_selector(".memnote", timeout=8000)
    conv = page.locator(".conv").inner_text()
    if "带上了长期记忆" in conv and "它最近自己做的事" in conv and "[记忆]" not in conv and "你好呀" in conv:
        ok("会话里：记忆说明、近况折叠成小标签，正文照常显示")
    else:
        fail("会话里记忆说明没折叠：" + conv[:300])
    page.screenshot(path=os.path.join(SHOTS, "conv-memory.png"))
    page.goto(url + "#/sessions/" + urllib.request.quote("@default/life", safe=""))
    page.wait_for_timeout(1500)
    page.screenshot(path=os.path.join(SHOTS, "conv-life.png"))
    if page.locator(".composer").count() == 0 and "内心活动" in page.locator(".innerbar").first.inner_text(timeout=3000):
        ok("内心会话只能看：没有输入框，顶上说明是它的内心活动")
    else:
        fail("内心会话还有输入框")
    page.screenshot(path=os.path.join(SHOTS, "conv-life.png"))
    page.goto(url + "#/")
    page.wait_for_selector("g.node.gateway")
    page.wait_for_timeout(600)
    if not page.locator('g.node[data-id="s:@default/life"]').count():
        ok("内心会话不占拓扑图的会话列")
    else:
        fail("内心会话出现在会话列里")
    # 收拾：改回工具型、删掉测试记忆
    api(page, "PATCH", "/api/agents/default", {"style": "tool"})
    for pth in ["people/张三.md", "国庆去成都.md"]:
        api(page, "DELETE", "/api/agents/default/memory/item?path=" + urllib.request.quote(pth))
    api(page, "PUT", "/api/agents/default/memory/self", {"text": ""})


if __name__ == "__main__":
    main()
