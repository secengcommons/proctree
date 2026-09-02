NR == 1 {
    if ($0 != "mode: atomic") exit 2
    next
}
NF != 3 { exit 2 }
{
    key = $1
    if (key in statements && statements[key] != $2) exit 2
    statements[key] = $2
    counts[key] += $3
}
END {
    for (key in statements) {
        total += statements[key]
        if (counts[key] == 0) missed += statements[key]
    }
    if (NR < 2 || total == 0) exit 2
    if (missed != 0) exit 1
}
