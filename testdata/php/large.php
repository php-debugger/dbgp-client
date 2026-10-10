<?php
// Debuggee for the MCP size limits: long output, values, stacks and lists.
// Line numbers are referenced by the tests: keep edits at the end of the file.

function down($n)
{
    if ($n === 0) {
        return 0;
    }
    return down($n - 1) + 1;
}

extract(array_fill_keys(array_map(fn ($i) => "v$i", range(1, 60)), 1));
$text = str_repeat("é", 50000);
echo $text;
for ($i = 1; $i <= 60; $i++) {
    trigger_error("warning $i", E_USER_WARNING);
}
for ($i = 1; $i <= 10; $i++) {
    trigger_error("again", E_USER_WARNING);
}
$line = 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx';
down(60);
