#!/bin/bash

key= [ foo faa fuu fii fee fyy ]

genline ()
{
    echo "$(date +%s%N)-$1" | sha256sum - |cut -c 2-52
}

for i in {1..50}; do
    genline "$( date +%N ; echo "$key[i]" |md5sum - )"
done

exit 0
