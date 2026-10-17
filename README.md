# ext-parser-python

Python parser plugin for **Onfly**. Built on [gotreesitter](https://github.com/odvcencio/gotreesitter) (pure-Go tree-sitter, no CGO) with the tree-sitter-python grammar. Compiles to a WASI module for Onfly's wazero sandbox, or a native binary for stdio use.

```bash
onfly ext install python
```

Handles `.py` and `.pyi`.

## Chunks

Input is one file on stdin, argv[1] is its path, output is the standard `[]Chunk` JSON.

| `type` | Source node | Notes |
|---|---|---|
| `import` | `import`, `from ... import`, `from __future__ import` | `name` = the statement |
| `class` | `class_definition` | header only (comments, decorators, signature, docstring); members are separate chunks |
| `function` `method` | `def` / `async def` | whole definition including decorators; `method` when directly in a class body. Nested functions stay inside their parent |
| `constant` `variable` | module-level `name = value` | `constant` when the name is UPPER_CASE |
| `property` | class-body `name = value` / `name: T = value` | `receiver` = owning class |
| `type` | `type X = ...` | |
| `statement` | other top-level code | consecutive statements merged |
| `file` | whole file | fallback when nothing else matched |

Metadata: `name`, `fqn` (dotted: `Outer.Inner.method`), `receiver`, `start_line`, `end_line`, and where they apply `signature`, `params`, `return_type`, `decorators`, `modifiers` (`async`), `extends`, `type`, `part`.

`#` comments directly above a declaration and decorators are part of the chunk. Declarations inside `if` / `try` / `with` blocks (`if TYPE_CHECKING:`, `try: import x`) are chunked like top-level ones. Bodies over 8 KB are split at line boundaries (`part=1/3`). Files over 48 KiB are not parsed: they come back as plain `file` chunks (`reason=too_large`), and files over 2 MiB yield `[]`. The host cancels a parse at 5 s (and then keeps the stale chunks); a 48 KiB file takes about 1.5 s on a laptop and about 3.5 s on a CI runner. Syntax errors do not fail the file: what parsed is chunked and a warning goes to stderr.

## Known limitations

- The wasm is about 18 MB (tree-sitter runtime plus the Python grammar), so expect a few seconds to compile it once per host start.
- Assignments with tuple or attribute targets (`a, b = ...`, `self.x = ...`) are not split out; at module level they fall into `statement`.

## Build

```bash
make wasm     # bin/python-parser.wasm (reproducible, Go 1.27+)
make native   # bin/python-parser
make test
```

## Test

```bash
make test                                                    # unit and golden tests
go test -tags "grammar_subset grammar_subset_python" -run Golden -update    # regenerate testdata/*.golden.json after an intended change
make wasm && PY_WASM=bin/python-parser.wasm go test -tags "grammar_subset grammar_subset_python" -run TestWasm -v .   # built module under wazero
```

The golden files in `testdata/` pin content and metadata of every chunk. The wazero test checks the chunk types, the 25 MiB size budget, and that the largest file we still parse stays under the host's 5 s limit. CI also runs a short fuzz pass and builds the wasm twice to check the bytes match.

The chunk contract is described in the [`cli` docs](https://github.com/OnFlyApp/cli/blob/main/docs/parsers.md). Plugins are listed in the [registry](https://github.com/OnFlyApp/registry).

## Use

```yaml
indexer:
  include: ["**/*.py"]
  parsers:
    - extensions: [".py", ".pyi"]
      type: "wasm"
      path: ".onfly/plugins/python-parser.wasm"
```

## Author

[Oleksii Ivanov](https://github.com/axitdev)

## License

Licensed under the **Functional Source License, Version 1.1, ALv2 Future License** (SPDX: `FSL-1.1-ALv2`), see [LICENSE](LICENSE).

Free for any purpose other than a competing use: making the software available in a commercial product or service that substitutes for it. Each version converts to Apache 2.0 two years after it is released.

Third-party licenses of the components embedded in the wasm: see THIRD_PARTY_NOTICES.
