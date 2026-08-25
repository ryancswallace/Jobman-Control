BEGIN {
    found = 0
}

$1 == "total:" {
    found = 1
    value = $3
    sub(/%$/, "", value)
    printf "aggregate statement coverage: %.1f%% (minimum %.1f%%)\n", value, minimum
    if ((value + 0) < (minimum + 0)) {
        exit 1
    }
}

END {
    if (!found) {
        print "coverage summary did not contain a total" > "/dev/stderr"
        exit 2
    }
}
