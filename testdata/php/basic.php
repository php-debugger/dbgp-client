<?php
// Debuggee used by the Xdebug fixture capture and integration tests.
// Line numbers are referenced by the tests: keep edits at the end of the file.

class Cache
{
    public $items = ['item1', 'item2', 'item3'];
}

class Service
{
    public $id = 42;
    protected $cache;
    private $name = 'svc';

    public function __construct()
    {
        $this->cache = new Cache();
    }
}

function add($a, $b)
{
    $sum = $a + $b;
    return $sum;
}

$name = "World";
$greeting = "Héllo, wörld";
$long = str_repeat("abcdefghij", 20);
$numbers = range(1, 40);
$user = ['name' => 'Alice', 'email' => 'alice@example.com', 'active' => true];
$obj = new Service();
$nullVal = null;
$float = 1.5;
$x = add(2, 3);
echo "x=$x\n";
for ($i = 0; $i < 5; $i++) {
    $y = $i * 2;
}
echo "done\n";
