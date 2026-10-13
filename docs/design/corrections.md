# 設計案: `deja corrections` — エージェントに教え直してきたことを掘り出す

状態: 提案（未実装） · 2026-10-13

## 1. 目的

人がエージェントを訂正したターン（「違う、そうじゃなくて〜」「前も言ったけど〜」）は、
その人しか持っていない知識が、使い方の文脈付きで書かれた場所である。本人が言語化できていない
ドメイン知識でも、AIの間違いを正すときには具体的に書かれている。

`deja corrections` はこの訂正を履歴から抽出し、**複数のセッションで繰り返し教え直している
こと**を候補として一覧にする。人が選んだ候補だけを `promote` して来歴付きのノートにし、
以後はノートとして既存の recall 経路でエージェントに届ける。

## 2. 主張の範囲（上流の方針に合わせる）

`cmd/deja/friction.go` と `cmd/deja/environment.go` のコメントが示す上流の方針は
「deja が言える最も狭い真の主張だけを言う」で、「試して却下した手法」の自動検出（#527, #541）は
外れたときに告発になるため見送られている。この機能も同じ線を守る。

- 言うこと: 「N 個の別セッションで、あなたはエージェントにこういう訂正をした」
- 言わないこと: 「これはあなたのドメイン知識だ」「これは正しい」
- **未承認の候補はエージェントに自動注入しない。** 訂正そのものが状況依存だったり、
  後で撤回されていたりする。エージェントに届くのは人が `promote` したノートだけ。

## 3. 抽出パイプライン

LLM も埋め込みも使わないルールベース。ゼロ依存の方針を崩さない。

### 3.1 候補ターン

次をすべて満たす `user` ロールのメッセージ。

1. `notAsked()`（`internal/index/ingest.go`）に該当しない。フックの封筒、割り込み通知、
   コンパクション要約、`<deja-recall` などの配管を除くため、既存の関数をそのまま再利用する
2. 同じセッション内で、直前に `assistant` の発話か `tool-output` がある（訂正には訂正される側が要る）
3. セッション最初の user ターンではない
4. 長さが 8〜400 rune（貼り付けたログや長いレポートを除く。`looksLikeQuestion` と同じ考え方）

### 3.2 シグナルとスコア

真偽値ではなく加点方式。**単独では弱いシグナルを組み合わせる**。

| シグナル | 例 | 重み |
|---|---|---|
| A. 冒頭の訂正マーカー | 違う / ちがう / そうじゃなくて / じゃなくて / いや、 / だから / 前も言った / さっき言った / 何度も / 勝手に / 戻して ・ no, / that's wrong / not what I / I said / I told you / don't / revert / actually ・ нет / не так | 2 |
| B. 直後のエージェントの謝罪・同意 | 申し訳 / すみません / おっしゃる通り / ご指摘 ・ You're right / Sorry / I apologize / Good catch | **3** |
| C. 規範的な内容 | 〜して / 〜しないで / 〜を使って / 〜は〜です ・ use X instead / always / never / should | 1 |
| D. 直後の取り消し操作 | `edit` レコードで直前の編集を戻す、`command` で `git checkout` / `git restore` / `git revert` | 1 |
| E. 苛立ちの強さ（ブチギレ度） | ！！ / は？ / 何回 / 全角感嘆符の連続 / 大文字の連続 / wtf | 0（表示専用） |

採用条件は **スコア 4 以上、かつ A か B のどちらかを含むこと**。

- B を最も重くするのは、エージェントが訂正されたときに謝罪から返す振る舞いがハーネスをまたいで
  安定しているため。人側の言い回しは多様だが、AI側の反応は定型に近い
- E はランキングに使わない。怒りの強さと知識の価値は比例しないため。ただし表示はする（名前の由来でもある）

### 3.3 抽出する単位

1件の訂正は次の3つ組として持つ。

```
assumed:   直前の assistant 発話の要旨（先頭 120 rune、または最後の 1 文）
corrected: user の訂正文から冒頭マーカーを剥がしたもの
evidence:  session key, harness, project, time, score, anger
```

「AIはこう思い込んだ → 私はこう正した」という対が、暗黙知の最小単位になる。
`corrected` だけでは「bun を使って」としか残らず、何に対する正解だったのかがわからない。

### 3.4 種類の推定（任意表示）

- `domain`: ファイルパス、コード識別子、固有名詞、数値・単位を含む
- `workflow`: エージェントの振る舞いを指す動詞が中心（コミット、push、勝手に、確認して、聞いて）

どちらも価値はあるが、ポストが言う「ドメイン知識」は前者。`--kind domain` で絞れるようにする。

## 4. 繰り返しの検出（クラスタリング）

`friction` は完全一致の行ハッシュで束ねるが、訂正は毎回言い回しが変わるので完全一致では束ねられない。

- 正規化: 小文字化、句読点を除去、冒頭マーカーを除去
- shingle: CJK は既存の `cjkBigrams()`（`internal/index/cjk.go`）、ラテン文字・キリル文字は単語。
  `cjkFunctionBigram()` で機能語のバイグラムを落とす
- 類似度: shingle 集合の Jaccard 係数が 0.5 以上なら同じクラスタ
- 規模: 候補は1セッションあたり数件しか出ない想定。数千件なら O(n²) でも 1 秒以内に収まる見込み。
  超えた場合は MinHash + LSH にする（Phase 2）
- 既定では **2 セッション以上** で「繰り返し」とする。friction の 3 より低くするのは、
  訂正が人の手で書かれたもので、エラー行より偶然の一致が起きにくいため。`--min-sessions` で変更可

参考: 既存の `questionStem()` は `strings.Fields` で5語未満を捨てるため、空白のない日本語の質問は
ほぼ全部落ちている可能性がある。この機能では使わないが、別件として要確認。

## 5. インターフェース

### 5.1 CLI

```text
$ deja corrections
エージェントに教え直してきたこと — 1149 セッションを読みました
  1. 4 sessions  bun を使って、pnpm は入ってない
                 AIの想定: "pnpm install で依存を入れます"
                 claude, codex · last Oct 2 · domain · 怒り ●●○
  2. 3 sessions  テストは make test じゃなくて go test ./... -race で
                 AIの想定: "make test を実行します"
                 claude · last Sep 28 · domain · 怒り ●○○
  3. 3 sessions  勝手に push しないで、確認してから
                 claude · last Sep 30 · workflow · 怒り ●●●

ノートにする: deja corrections --promote 1
```

| フラグ | 内容 |
|---|---|
| `--project`, `--harness`, `--since` | 既存の検索と同じフィルタ |
| `--kind domain\|workflow` | 種類で絞る |
| `--all` | 1 セッションだけの、スコアが高い訂正も出す |
| `--min-sessions n` | 繰り返しとみなす閾値 |
| `--limit n`, `--json` | 既存の慣例どおり |
| `--promote <n> [--text "…"] [--tag t]` | 候補をノートにする（5.2） |

結果がゼロのときの文言は `runFriction` に倣い、「インデックスが空」「user ターンがない」
「閾値に届かない」を区別して出す（#637, #705 と同じ理由）。

### 5.2 promote

`--promote` は既存の notes 機構（`internal/sources/notes.go`）に書き込む。

- `text`: `誤: <assumed>\n正: <corrected>`。`--text` で人が書き直せる。**書き直しを推奨する**
- `tags`: `correction` と、種類（`domain` / `workflow`）
- `state`: `accepted`
- 来歴: クラスタに含まれる全セッションの id

以降は通常のノートとして扱われる。検索で生の会話より上位に出て、MCP の recall で
エージェントに届き、`promote --state rejected` で撤回できる。新しい経路は作らない。

### 5.3 MCP / 自動注入

- Phase 1 では **MCP ツールもセッション開始時の注入も追加しない**（2 章の理由）
- 既存の `deja` 起動画面（`cmd/deja/brief.go`）には、friction の `again` 行と並べて
  「教え直し 4 回: bun を使って… · deja corrections」を 1 行だけ出す（Phase 2）

## 6. ポリシー・プライバシー

- 全面で `policy.Allows(activation, project)` を通す。`environmentBlock` が #659 で漏れた
  前例があるので、クラスタ単位ではなくセッション単位で絞り、絞った後の件数で閾値を判定し直す
- レコードは索引時にマスキング済み（`redact`）なので、新たなマスキング処理は不要
- 苛立ちの表現は個人的なものなので、`share`、統計カード、`view` の公開用出力には含めない
- `forget` / トゥームストーンで消えたセッションは、当然候補にも出ない（索引を読むだけなので自動で守られる）

## 7. 実装の分割

### Phase 1 — コマンド単体（manifest は変更しない）

| ファイル | 内容 |
|---|---|
| `internal/index/corrections.go` | `DetectCorrection(prev, user, next []Record) (Correction, bool)`、スコア、shingle、クラスタリング |
| `internal/index/corrections_markers.go` | 言語別のマーカー表（ja / en / ru） |
| `internal/index/store_io.go` | `EachSessionRecords(dir, fn func(SessionMeta, []Record))` — セッション単位で全ロールを順に渡す。既存の `EachRecordOfRole` は1ロールしか流さないので、前後関係が取れない |
| `cmd/deja/corrections.go` | コマンド本体、表示、`--promote` |
| `cmd/deja/main.go`, `completion.go` | コマンドの登録 |
| `README.md`, `docs/guide/commands.html` | コマンド表に追記 |

### Phase 2 — 起動画面と高速化

- `SessionMeta` に `Taught []uint64`（訂正の MinHash 署名）を追加し、索引時に計算する。
  friction の `Hit` と同じ作りで、起動画面がノーコストで件数を出せるようになる
- manifest のバージョンを上げる（`version_upgrade_test.go` の流儀に従う）
- `brief.go` に 1 行を追加する

### Phase 3 — 検討事項（今は作らない）

- promote 済みの `correction` ノートを、プロジェクト単位でセッション開始時の注入に含める
- 訂正の後に、同じ内容で再び訂正している（＝ノートが効いていない）ことの検出

## 8. 検証

上流は「計測した」を判断の根拠にしているので、実装前に評価の土台を作る。

1. **ラベル付きデータ**: 自分の索引から、候補条件 3.1 を満たす user ターンを 300 件抽出し、
   訂正か否かを人手でラベル付けする。個人情報を含むためリポジトリには入れず、
   合成した 40 件程度を `fixtures/corrections/` に置く
2. **指標**: 上位 20 件の適合率を **80% 以上**（取りこぼしより誤検出を嫌う。
   `looksLikeQuestion` のコメントと同じ判断）。再現率は参考値として記録する
3. **単体テストで押さえる誤検出**
   - 「いやー助かった」「いいや、それで合ってる」— マーカーはあるが訂正ではない → B も C もないので落ちる
   - 「次は README を直して」— 普通の次の指示 → A も B もないので落ちる
   - フックが注入したテキスト、コンパクション要約、`<system-reminder>` → `notAsked` で落ちる
   - deja 自身の `corrections` 出力をエージェントが読んだもの → friction の自己学習除外と同様に、出力形式で除外する
   - サブエージェントへのプロンプト（Claude Code の sidechain が user ロールで記録される場合）
     → パーサが sidechain を落としているか**要確認**
4. **ツール**: `scripts/correctionsprobe`（`sample` → `label` → `eval`）。判定は
   `internal/index/corrections.go` の `ScoreCorrection` をそのまま使う。sample の出力は既定で
   `~/deja-corrections-sample.jsonl`（リポジトリの外）。eval は件数と比率しか出さない
5. **性能**: 1000 セッションの索引で 2 秒以内（`friction` が 1 パス化で 2m46s から縮めた経緯があるので、
   セッション単位の読み直しはしない）

## 9. 未確定事項

- `records.bin` で同じセッションのレコードが連続し、時刻順に並んでいるか。
  前提が崩れる場合は `EachSessionRecords` 内でキーごとにまとめて時刻順に並べ替える
- 謝罪マーカー（B）がハーネス・モデルごとにどれだけ安定しているか。Codex や Gemini の
  言い回しを実データで確認する
- コマンド名。`corrections` は中立的だが、`taught`（「教えたこと」）のほうが目的は伝わる

## 10. 位置づけ

このフォーク（sora-bluesky/deja-vu）独自の機能とし、上流（vshulcz/deja-vu）には提案しない。
上流との差分を小さく保つため、変更は新規ファイルに寄せ、既存ファイルへの変更は
コマンド登録とドキュメントへの追記に留める（Phase 2 の manifest 拡張は、上流の取り込み時に
衝突しやすいので慎重に判断する）。

## 11. 計測記録

### 1回目（2026-10-13, 8101 セッション / user ターン 49552）

- 候補 6492、判定通過 45（0.7%）。低すぎるため、ラベル付けの前に `stats` で原因を調べた
- 長さ・配管の条件で 33994 件（69%）が落ちていた。8〜400 rune の範囲が狭すぎた
- 最多のマーカーは `stop`（33件）で、`Stop hook feedback:` などフックのテキストへの誤反応と見られる
- 同じ文面の指示がループで繰り返し送られ、返答「公開まで止めずに進めます」が 9 回出ていた
- 謝罪の最多は「ご指摘」63 件で、「申し訳」は 2 件。最近のモデルは謝らずに礼を言う
- 日本語の訂正マーカーが少なく、マーカーに当たったのは候補の 1.5% だけ

### 改訂（v2）

- 長さを 4〜1000 rune に広げ、マーカーは冒頭 300 rune だけで探す
- フック・スキル展開などの配管を除外（`correctionPlumbing`）。同じ文面が 5 セッション以上に出る
  ターンは自動送信とみなして除外（probe 側）
- マーカーを種類別にした: negation / repeat / unasked（重み 3）、not-fixed / undo（2）、why（1）
- 謝罪表に「ご指摘」系・「誤り」「勘違い」「見落と」などを追加
- 採用条件: スコア 3 以上、かつマーカーか謝罪のどちらかを含む。言い切った訂正（negation など）は単独で通る
