# Contributing to fma

All forms of pull requests are welcome: code, fixes, features, documentation,
translations, tests, deployment, CI, performance work, accessibility improvements,
and changes or additions to any part of the project. Small changes, large changes,
experiments and draft PRs are welcome. You do not need permission to propose work.

AI-assisted programming is welcome, including substantially AI-generated changes.
We review the resulting work, not which editor, model or tool produced it. Review
what you submit, explain its behavior, and report the checks you actually ran.
AI use does not require a special label or a separate approval process.

## The single-file philosophy

**The one non-negotiable architectural constraint is the single-file philosophy.**
Keep all fma runtime Go code in `main.go` and Go tests in `main_test.go`. Extend and
refactor these files as needed; do not split the application into additional Go
files, internal packages, generated Go sources, plugins or embedded source files
that merely hide a split implementation. Reusing third-party protocol libraries is
encouraged. Use explicit types and JSON tags for fma-owned wire and storage fields.

Shell/Python integration tests, deployment templates, workflows, documentation and
other support files may live separately. This constraint is about the application's
implementation, not forcing deployment or documentation into Go. Keep Markdown
base names uppercase, for example `README.md`, `README.ZH-CN.md`,
and `CONTRIBUTING.md`.

Every part of the implementation and documentation is open to change within this
constraint. Welcoming a PR does not promise automatic merging: maintainers still
review correctness and discuss how the change should fit.

## Preparing a change

Describe the problem and the resulting behavior. Include a small example when it
makes the change easier to review. Document material compatibility changes, and
keep `README.md` English-only and `README.ZH-CN.md` in Chinese. Update both when
behavior, configuration, deployment or protocol support changes.

Use Go 1.25 or newer. Install the native Fals3y binary for integration tests; Docker
is not needed. From the repository root:

```sh
make test
make build
```

Run the JMAP integration suite directly with:

```sh
python3 scripts/verify_jmap.py
```

`make test` includes formatting, the single-file check, vet, race tests, native S3
protocol integration tests and installer/deployment tests. Add meaningful checks
for changed behavior. For documentation-only edits, check links, examples and
configuration names; no new unit tests are needed. If a check cannot run, state
which one and why rather than reporting it as passed.

Keep commits focused and explain what changed. Submit the PR in whatever form is
useful for review, including a draft when discussion would help. Contributions are
provided under the project's [MIT license](LICENSE); preserve applicable upstream
copyright and license notices.

## 中文说明

欢迎任何形式的 PR，以及对任何部分的修改和补充，包括代码、功能、修复、文档、翻译、测试、部署、CI 和性能优化。欢迎小改动、大改动、实验和草稿 PR，不需要事先申请。

明确接受 AI 辅助编程，包括主要由 AI 生成的代码。按最终成果审查，不按使用的工具区别对待；提交者应检查内容、解释行为，并如实说明运行过的验证，不要求额外的 AI 标记或审批。

**唯一不可破坏的架构约束是单文件哲学：运行时 Go 实现放在 `main.go`，Go 测试放在 `main_test.go`。** 不要通过新增 Go 文件、内部包、生成代码或嵌入源码绕过它。鼓励复用第三方库，fma 自有协议和持久化字段使用明确类型与 JSON 标签。脚本、模板、工作流和文档可以独立存放。

Markdown 文件基本名使用大写；`README.md` 只写英文，中文版写在 `README.ZH-CN.md`。行为、参数、部署或协议变化时同步更新两版。接受提交不等于自动合并，仍需检查正确性并讨论具体方案。上述命令提供本地验证入口；如有检查无法执行，请如实说明。遵守项目 MIT 许可证及上游许可证。
