| id | note | note | amount | ok | doc |
| --- | --- | --- | --- | --- | --- |
| 1 | NULL |  | 12.50 | t | {"a": [1, 2]} |
| 2 | \NULL | \\N | NaN | f | NULL |
| 3 | tab	here | two\nlines\r\n | -0.001 | NULL | "x<y & z" |
| 4 | pipe\|here | say "hi", ok | -Infinity | t | null |
| 5 | back\\slash | café ☕ 日本語 | 12345678901234567890…[+10 chars] | f | [true, null] |
| 6 | a cell wider than tw…[+10 chars] | 日本語のテキストは二十文字を超えると切ら…[+3 chars] | NULL | t | {"k": "v"} |
