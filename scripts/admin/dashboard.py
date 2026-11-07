#!/usr/bin/env python3
"""ことづての管理画面を手元で作って開く。

インターネット上に管理用の入り口は置かない。このスクリプトが
  1. AWS の管理用 Lambda（kotozute-admin-stats / kotozute-admin-feedback）を
     あなたの IAM 権限で直接呼ぶ（API Gateway を通らない）
  2. App Store Connect の売上レポートから DL 数を取る（鍵はこの Mac から出ない）
  3. 結果を HTML にして、iCloud の外（~/Library/Application Support）に書き出して開く

使い方:
  python3 scripts/admin/dashboard.py           # 手元だけのサーバーを立てて開く（画面の「更新する」で取り直せる）
  python3 scripts/admin/dashboard.py --once    # HTML を1回だけ書き出して開く（サーバーを立てない）
  AWS_PROFILE=xxx python3 scripts/admin/dashboard.py   # プロファイルを変えるとき

サーバーは 127.0.0.1 だけで待ち受け、起動ごとに作るランダムな合言葉を URL に入れる。
Host ヘッダが 127.0.0.1 / localhost でなければ断る（DNS リバインディング対策）。
止めるときは Ctrl+C。サーバーで動かすときは HTML をファイルに残さない。

設定は ~/.config/kotozute-admin/config.json（リポジトリには置かない）:
  {
    "ascKeyId": "...", "ascIssuerId": "...",
    "ascKeyPath": "~/.appstoreconnect/private_keys/AuthKey_XXXX.p8",
    "vendorNumber": "8xxxxxxx",      # App Store Connect の「支払いと財務レポート」左上の番号
    "appleId": "6807755962",
    "salesSince": "2026-10-01"
  }
vendorNumber が無ければ DL 数は出さずに、残りだけ表示する。
"""
import base64
import datetime as dt
import gzip
import html
import http.server
import json
import os
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

CONFIG_PATH = os.path.expanduser("~/.config/kotozute-admin/config.json")
# 出力先。iCloud（~/Documents, ~/Desktop）の外で、リポジトリの外。
OUT_DIR = os.path.expanduser("~/Library/Application Support/kotozute-admin")
REGION = "ap-northeast-1"
JST = dt.timezone(dt.timedelta(hours=9))


# ---------------------------------------------------------------- AWS

def aws_env():
    """ローカル開発用のダミーキー（local/local）が環境に残っていたら外す。
    残っていると本番の Lambda を呼べないうえ、何が起きたか分かりにくい。"""
    env = dict(os.environ)
    if env.get("AWS_ACCESS_KEY_ID") == "local":
        for k in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"):
            env.pop(k, None)
    env["AWS_REGION"] = REGION
    env["AWS_DEFAULT_REGION"] = REGION
    return env


def invoke(function_name):
    with tempfile.TemporaryDirectory() as tmp:
        out_path = os.path.join(tmp, "out.json")
        res = subprocess.run(
            ["aws", "lambda", "invoke", "--function-name", function_name,
             "--cli-binary-format", "raw-in-base64-out", "--payload", "{}",
             out_path],
            env=aws_env(), capture_output=True, text=True,
        )
        if res.returncode != 0:
            raise RuntimeError(f"{function_name} を呼べませんでした: {res.stderr.strip()}")
        meta = json.loads(res.stdout or "{}")
        with open(out_path) as f:
            payload = json.load(f)
        if meta.get("FunctionError"):
            raise RuntimeError(f"{function_name} がエラーを返しました: {payload}")
        return payload


# ---------------------------------------------------------------- App Store Connect

def b64url(data):
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def asc_token(cfg):
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec, utils

    with open(os.path.expanduser(cfg["ascKeyPath"]), "rb") as f:
        key = serialization.load_pem_private_key(f.read(), password=None)
    now = int(time.time())
    header = {"alg": "ES256", "kid": cfg["ascKeyId"], "typ": "JWT"}
    payload = {"iss": cfg["ascIssuerId"], "iat": now, "exp": now + 600, "aud": "appstoreconnect-v1"}
    signing_input = f"{b64url(json.dumps(header).encode())}.{b64url(json.dumps(payload).encode())}"
    r, s = utils.decode_dss_signature(key.sign(signing_input.encode(), ec.ECDSA(hashes.SHA256())))
    return f"{signing_input}.{b64url(r.to_bytes(32, 'big') + s.to_bytes(32, 'big'))}"


def fetch_daily_units(cfg, token, day):
    """その日の売上レポート（SUMMARY）から、このアプリの行だけを数える。
    戻り値: {"downloads": 新規DL, "redownloads": 再DL, "updates": アップデート}
    Product Type Identifier: 1*=新規 / 3*=再ダウンロード / 7*=アップデート"""
    query = urllib.parse.urlencode({
        "filter[frequency]": "DAILY",
        "filter[reportDate]": day.isoformat(),
        "filter[reportSubType]": "SUMMARY",
        "filter[reportType]": "SALES",
        "filter[vendorNumber]": cfg["vendorNumber"],
        "filter[version]": "1_1",
    })
    req = urllib.request.Request(
        f"https://api.appstoreconnect.apple.com/v1/salesReports?{query}",
        headers={"Authorization": f"Bearer {token}", "Accept": "application/a-gzip"},
    )
    counts = {"downloads": 0, "redownloads": 0, "updates": 0}
    try:
        with urllib.request.urlopen(req) as res:
            text = gzip.decompress(res.read()).decode("utf-8")
    except urllib.error.HTTPError as e:
        if e.code == 404:  # その日は実績なし（またはまだ集計前）
            return counts
        raise
    lines = text.strip().split("\n")
    head = lines[0].split("\t")
    col = {name: i for i, name in enumerate(head)}
    for line in lines[1:]:
        f = line.split("\t")
        if f[col["Apple Identifier"]] != str(cfg.get("appleId", "")):
            continue
        units = int(float(f[col["Units"]] or 0))
        kind = f[col["Product Type Identifier"]][:1]
        if kind == "1":
            counts["downloads"] += units
        elif kind == "3":
            counts["redownloads"] += units
        elif kind == "7":
            counts["updates"] += units
    return counts


def load_sales(cfg):
    """日別の DL 数。取った日はキャッシュし、直近3日だけ取り直す（遅れて載ることがある）。"""
    if not cfg.get("vendorNumber"):
        return None
    cache_path = os.path.join(OUT_DIR, "sales-cache.json")
    try:
        with open(cache_path) as f:
            cache = json.load(f)
    except FileNotFoundError:
        cache = {}

    token = asc_token(cfg)
    start = dt.date.fromisoformat(cfg.get("salesSince", "2026-10-01"))
    # 日別レポートは翌日以降に出る。今日の分はまだ無い。
    end = dt.datetime.now(JST).date() - dt.timedelta(days=1)
    # Apple が日別レポートを残すのは約1年。
    start = max(start, end - dt.timedelta(days=364))
    recent = end - dt.timedelta(days=3)
    day = start
    while day <= end:
        key = day.isoformat()
        if key not in cache or day >= recent:
            cache[key] = fetch_daily_units(cfg, token, day)
        day += dt.timedelta(days=1)

    with open(cache_path, "w") as f:
        json.dump(cache, f, indent=1, sort_keys=True)
    os.chmod(cache_path, 0o600)
    return {k: v for k, v in sorted(cache.items()) if start.isoformat() <= k <= end.isoformat()}


# ---------------------------------------------------------------- HTML

def esc(value):
    return html.escape(str(value), quote=True)


def fmt_jst(iso):
    try:
        t = dt.datetime.fromisoformat(iso.replace("Z", "+00:00")).astimezone(JST)
    except ValueError:
        return esc(iso)
    return t.strftime("%Y/%m/%d %H:%M")


def week_label(iso_date):
    d = dt.date.fromisoformat(iso_date)
    return f"{d.month}/{d.day}"


def bar_chart(points, unit):
    """1系列の棒グラフ（SVG）。points = [(ラベル, 値)]。
    系列が1つなので凡例は置かない（見出しが名前を言う）。値はホバーで出す。"""
    if not points:
        return '<p class="muted">データがありません。</p>'
    w, h, pad_l, pad_b, pad_t = 640, 200, 36, 24, 12
    plot_w, plot_h = w - pad_l - 8, h - pad_b - pad_t
    peak = max(v for _, v in points) or 1
    # 目盛りはきりのいい数に。
    step = max(1, 10 ** (len(str(peak)) - 1))
    while peak / step > 5:
        step *= 2
    top = ((peak + step - 1) // step) * step
    slot = plot_w / len(points)
    bar_w = max(2, min(36, slot - 2))  # 隣の棒とのあいだに2px以上の隙間
    out = [f'<svg viewBox="0 0 {w} {h}" class="chart" role="img">']
    for tick in range(0, top + 1, step):
        y = pad_t + plot_h - plot_h * tick / top
        out.append(f'<line x1="{pad_l}" x2="{w - 8}" y1="{y:.1f}" y2="{y:.1f}" class="grid"/>')
        out.append(f'<text x="{pad_l - 6}" y="{y + 4:.1f}" class="axis" text-anchor="end">{tick}</text>')
    label_every = max(1, len(points) // 8)
    for i, (label, value) in enumerate(points):
        x = pad_l + slot * i + (slot - bar_w) / 2
        bh = plot_h * value / top
        y = pad_t + plot_h - bh
        tip = esc(f"{label}：{value}{unit}")
        # ホバーの当たり判定は棒より広く（スロット全体）。
        out.append(f'<g class="hit" data-tip="{tip}"><rect x="{pad_l + slot * i:.1f}" y="{pad_t}" width="{slot:.1f}" height="{plot_h}" class="hitbox"/>')
        if value > 0:
            r = min(4, bar_w / 2, bh)
            # 上端だけ丸め、基線側は角のまま。
            out.append(
                f'<path class="bar" d="M{x:.1f},{y + bh:.1f} V{y + r:.1f} Q{x:.1f},{y:.1f} {x + r:.1f},{y:.1f} '
                f'H{x + bar_w - r:.1f} Q{x + bar_w:.1f},{y:.1f} {x + bar_w:.1f},{y + r:.1f} V{y + bh:.1f} Z"/>'
            )
        out.append("</g>")
        if i % label_every == 0 or i == len(points) - 1:
            out.append(f'<text x="{x + bar_w / 2:.1f}" y="{h - 6}" class="axis" text-anchor="middle">{esc(label)}</text>')
    out.append(f'<line x1="{pad_l}" x2="{w - 8}" y1="{pad_t + plot_h}" y2="{pad_t + plot_h}" class="baseline"/>')
    out.append("</svg>")
    return "".join(out)


def tile(label, value, note=""):
    note_html = f'<div class="note">{esc(note)}</div>' if note else ""
    return f'<div class="tile"><div class="label">{esc(label)}</div><div class="value">{esc(value)}</div>{note_html}</div>'


def weekly_downloads(sales):
    weeks = {}
    for day, c in sales.items():
        d = dt.date.fromisoformat(day)
        monday = d - dt.timedelta(days=d.weekday())
        weeks[monday.isoformat()] = weeks.get(monday.isoformat(), 0) + c["downloads"]
    return sorted(weeks.items())


def render(stats, feedback, sales, sales_error, live=False):
    weekly = stats.get("weekly", [])
    tiles = [
        tile("ユーザー", stats["users"]),
        tile("ペア", stats["pairs"]),
        tile("ことづて", stats["kotozute"]),
        tile("7日以内に書いた人", stats["activeWriters7d"], f"30日以内：{stats['activeWriters30d']}人"),
        tile("ご意見", stats["feedback"]),
    ]
    if sales is not None:
        total = sum(c["downloads"] for c in sales.values())
        last7 = sum(c["downloads"] for c in list(sales.values())[-7:])
        tiles.insert(0, tile("ダウンロード", total, f"直近7日：{last7}"))

    sections = []
    sections.append(f'<section><h2>週ごとのことづて</h2>{bar_chart([(week_label(w["weekStart"]), w["kotozute"]) for w in weekly], "枚")}</section>')

    if sales is not None:
        dl_weeks = weekly_downloads(sales)
        sections.append(f'<section><h2>週ごとのダウンロード</h2>{bar_chart([(week_label(k), v) for k, v in dl_weeks[-12:]], "件")}'
                        '<p class="muted">App Store Connect の売上レポート（新規ダウンロードのみ。再ダウンロード・アップデートは含みません）。</p></section>')
    else:
        msg = esc(sales_error) if sales_error else "~/.config/kotozute-admin/config.json に vendorNumber を入れると表示されます。"
        sections.append(f'<section><h2>ダウンロード</h2><p class="muted">{msg}</p></section>')

    rows = "".join(
        f'<tr><td>{week_label(w["weekStart"])}〜</td><td>{w["kotozute"]}</td><td>{w["newUsers"]}</td><td>{w["newPairs"]}</td><td>{w["feedback"]}</td></tr>'
        for w in reversed(weekly)
    )
    missing = ""
    if stats.get("usersWithoutCreatedAt") or stats.get("pairsWithoutCreatedAt"):
        missing = (f'<p class="muted">登録日時を持たない古いデータ（ユーザー {stats["usersWithoutCreatedAt"]}・'
                   f'ペア {stats["pairsWithoutCreatedAt"]}）は、新規の列に入りません。</p>')
    sections.append(f'<section><h2>週ごとの表</h2><table><thead><tr><th>週（月曜〜）</th><th>ことづて</th><th>新規ユーザー</th><th>新規ペア</th><th>ご意見</th></tr></thead><tbody>{rows}</tbody></table>{missing}</section>')

    items = feedback.get("items", [])
    if items:
        fb = "".join(
            f'<li><div class="meta">{fmt_jst(i["createdAt"])}　{esc(i.get("appVersion", ""))}　<code>{esc(i["userId"])}</code></div>'
            f'<div class="body">{esc(i["body"])}</div></li>'
            for i in items
        )
        fb_html = f'<ul class="feedback">{fb}</ul>'
    else:
        fb_html = '<p class="muted">まだありません。</p>'
    sections.append(f'<section><h2>ご意見（{len(items)}件）</h2>{fb_html}</section>')

    generated = fmt_jst(stats["generatedAt"])
    return f"""<!DOCTYPE html>
<html lang="ja"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>ことづて 管理</title>
<style>
:root {{ --bg:#f6f5f2; --surface:#fcfcfb; --ink:#1f1e1c; --ink-soft:#5f5d58; --line:#e4e2dc; --bar:#2a78d6; }}
@media (prefers-color-scheme: dark) {{
  :root {{ --bg:#121211; --surface:#1a1a19; --ink:#eceae5; --ink-soft:#a3a09a; --line:#33322f; --bar:#3987e5; }}
}}
* {{ box-sizing:border-box; }}
body {{ margin:0; background:var(--bg); color:var(--ink); font:15px/1.6 -apple-system, "Hiragino Sans", sans-serif; }}
main {{ max-width:880px; margin:0 auto; padding:24px 16px 64px; }}
h1 {{ font-size:22px; margin:0 0 4px; }}
h2 {{ font-size:16px; margin:0 0 12px; }}
.muted, .note {{ color:var(--ink-soft); font-size:13px; }}
.tiles {{ display:grid; grid-template-columns:repeat(auto-fill,minmax(150px,1fr)); gap:12px; margin:20px 0; }}
.tile, section {{ background:var(--surface); border:1px solid var(--line); border-radius:12px; padding:14px 16px; }}
section {{ margin-top:16px; }}
.tile .label {{ color:var(--ink-soft); font-size:13px; }}
.tile .value {{ font-size:28px; font-weight:600; font-variant-numeric:tabular-nums; }}
.chart {{ width:100%; height:auto; display:block; }}
.chart .grid {{ stroke:var(--line); stroke-width:1; }}
.chart .baseline {{ stroke:var(--ink-soft); stroke-width:1; }}
.chart .axis {{ fill:var(--ink-soft); font-size:11px; }}
.chart .bar {{ fill:var(--bar); }}
.chart .hitbox {{ fill:transparent; }}
.chart .hit:hover .hitbox {{ fill:var(--line); opacity:.5; }}
#tip {{ position:fixed; pointer-events:none; background:var(--ink); color:var(--surface); font-size:12px; padding:4px 8px; border-radius:6px; display:none; }}
table {{ width:100%; border-collapse:collapse; font-variant-numeric:tabular-nums; }}
th, td {{ text-align:right; padding:6px 8px; border-bottom:1px solid var(--line); }}
th:first-child, td:first-child {{ text-align:left; }}
th {{ color:var(--ink-soft); font-weight:500; font-size:13px; }}
.feedback {{ list-style:none; margin:0; padding:0; }}
.feedback li {{ padding:12px 0; border-bottom:1px solid var(--line); }}
.feedback li:last-child {{ border-bottom:none; }}
.feedback .meta {{ color:var(--ink-soft); font-size:12px; }}
.feedback .body {{ white-space:pre-wrap; overflow-wrap:anywhere; }}
code {{ font-size:11px; }}
.wrap {{ overflow-x:auto; }}
.head {{ display:flex; align-items:flex-start; justify-content:space-between; gap:12px; }}
.refresh {{ font:inherit; font-size:14px; padding:8px 16px; border-radius:999px; border:1px solid var(--line);
  background:var(--surface); color:var(--ink); cursor:pointer; white-space:nowrap; }}
.refresh:disabled {{ color:var(--ink-soft); cursor:default; }}
</style></head>
<body><main>
<div class="head"><div>
<h1>ことづて 管理</h1>
<div class="muted">{generated} 時点（日本時間）・この Mac だけで作った画面です</div>
</div>{'<button class="refresh" id="refresh">更新する</button>' if live else ''}</div>
<div class="tiles">{''.join(tiles)}</div>
{''.join(sections)}
</main>
<div id="tip"></div>
<script>
const tip = document.getElementById('tip');
document.querySelectorAll('.hit').forEach(g => {{
  g.addEventListener('mousemove', e => {{ tip.textContent = g.dataset.tip; tip.style.display = 'block';
    tip.style.left = (e.clientX + 12) + 'px'; tip.style.top = (e.clientY - 28) + 'px'; }});
  g.addEventListener('mouseleave', () => {{ tip.style.display = 'none'; }});
}});
const refresh = document.getElementById('refresh');
if (refresh) refresh.addEventListener('click', () => {{
  refresh.disabled = true; refresh.textContent = '更新中…'; location.reload();
}});
</script>
</body></html>"""


# ---------------------------------------------------------------- main

def load_config():
    try:
        with open(CONFIG_PATH) as f:
            return json.load(f)
    except FileNotFoundError:
        return {}


def collect(cfg, live):
    """AWS と App Store Connect から取り直して、画面の HTML を返す。"""
    print("集計を取得しています…", file=sys.stderr)
    stats = invoke("kotozute-admin-stats")
    feedback = invoke("kotozute-admin-feedback")

    sales, sales_error = None, None
    if cfg.get("vendorNumber"):
        print("App Store Connect から DL 数を取得しています…", file=sys.stderr)
        try:
            sales = load_sales(cfg)
        except Exception as e:  # DL 数が取れなくても、残りは見られるようにする
            sales_error = f"DL 数を取得できませんでした（{e}）"
    return render(stats, feedback, sales, sales_error, live=live)


def run_once(cfg):
    out_path = os.path.join(OUT_DIR, "index.html")
    # ご意見の本文が入るので、自分だけが読める権限で書く。
    fd = os.open(out_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(collect(cfg, live=False))
    os.chmod(out_path, 0o600)
    print(out_path)
    if "--no-open" not in sys.argv:
        subprocess.run(["open", out_path])


def serve(cfg):
    # 前に --once で書き出したものがあれば消す（ご意見の本文を手元に残さない）。
    try:
        os.remove(os.path.join(OUT_DIR, "index.html"))
    except FileNotFoundError:
        pass

    token = secrets.token_urlsafe(16)

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            port = self.server.server_address[1]
            # ブラウザ以外の名前（悪意のあるドメインを 127.0.0.1 に向けたもの）で来たら断る。
            if self.headers.get("Host") not in (f"127.0.0.1:{port}", f"localhost:{port}"):
                return self.reply(403, "forbidden")
            if self.path != f"/{token}/":
                return self.reply(404, "not found")
            try:
                body = collect(cfg, live=True)
                status = 200
            except Exception as e:
                body = f"<!DOCTYPE html><meta charset=utf-8><p>取得できませんでした：{esc(e)}</p>"
                status = 500
            self.reply(status, body, "text/html; charset=utf-8")

        def reply(self, status, body, content_type="text/plain; charset=utf-8"):
            data = body.encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            # 取り直すたびに最新を出す。ほかのページに埋め込ませない。
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Frame-Options", "DENY")
            self.send_header("Referrer-Policy", "no-referrer")
            self.send_header("Content-Security-Policy",
                             "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args):  # URL（合言葉入り）をログに出さない
            pass

    # 127.0.0.1 だけで待ち受ける。ポートは OS に空いているものを選ばせる。
    server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    url = f"http://127.0.0.1:{server.server_address[1]}/{token}/"
    print("管理画面を開きました。止めるときは Ctrl+C。", file=sys.stderr)
    if "--no-open" not in sys.argv:
        subprocess.run(["open", url])
    else:
        print(url)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n止めました。", file=sys.stderr)
    finally:
        server.server_close()


def main():
    os.makedirs(OUT_DIR, mode=0o700, exist_ok=True)
    os.chmod(OUT_DIR, 0o700)
    cfg = load_config()
    if "--once" in sys.argv:
        run_once(cfg)
    else:
        serve(cfg)


if __name__ == "__main__":
    main()
