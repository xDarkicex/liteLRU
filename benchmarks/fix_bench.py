with open("baseline_bench.go", "r") as f:
    text = f.read()

text = text.replace("func(key string, nil)", "func(key string)")

with open("baseline_bench.go", "w") as f:
    f.write(text)

with open("write_heavy_bench.go", "r") as f:
    text2 = f.read()

text2 = text2.replace("otterCache.Set(ops[j].key, nil, nil)", "otterCache.Set(ops[j].key, nil)")

with open("write_heavy_bench.go", "w") as f:
    f.write(text2)

