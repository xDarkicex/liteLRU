import os, glob

# Fix all .Get calls in go files
for root, _, files in os.walk("."):
    for f in files:
        if f.endswith(".go") and f != "liteLRU.go":
            path = os.path.join(root, f)
            with open(path, "r") as file:
                content = file.read()
            
            # For liteLRU copies in ablation/
            if "liteLRU.go" in f:
                # Same replacements as main liteLRU.go
                # 1. Remove paramSlicePools block
                start_pool = content.find("var paramSlicePools = [5]sync.Pool{")
                if start_pool != -1:
                    end_pool = content.find("func NewLRUCache(capacity, maxParams int) *LRUCache {")
                    content = content[:start_pool] + content[end_pool:]
                
                # 2. Fix Add
                add_old = """\t\t} else {
\t\t\tif oldParams != nil {
\t\t\t\tputParamSlice(oldParams)
\t\t\t}
\t\t\tnewParams = getParamSlice(len(params))
\t\t\tcopy(newParams, params)
\t\t}
\t} else {
\t\tif oldParams != nil {
\t\t\tputParamSlice(oldParams)
\t\t}
\t\tnewParams = nil
\t}
\tc.params[victimIdx].Store(newParams)"""
                add_new = """\t\t} else {
\t\t\tnewParams = make([]Param, len(params))
\t\t\tcopy(newParams, params)
\t\t}
\t} else {
\t\tnewParams = nil
\t}
\tc.params[victimIdx].Store(newParams)"""
                content = content.replace(add_old, add_new)

                # 3. Fix Get signature
                get_sig_old = "func (c *LRUCache) Get(method, path string) (HandlerFunc, []Param, bool) {"
                get_sig_new = "func (c *LRUCache) Get(method, path string, dst []Param) (HandlerFunc, []Param, bool) {"
                content = content.replace(get_sig_old, get_sig_new)

                # 4. Fix Get body
                get_body_old = """\tvar copiedParams []Param
\tif len(params) > 0 {
\t\tcopiedParams = getParamSlice(len(params))
\t\tcopy(copiedParams, params)
\t}

\t// Validate read seqlock
\tseq2 := c.states[idx].seq.Load()
\tif seq1 != seq2 {
\t\t// Slot was modified while we were reading!
\t\tif copiedParams != nil {
\t\t\tputParamSlice(copiedParams)
\t\t}"""
                get_body_new = """\tvar copiedParams []Param
\tif len(params) > 0 {
\t\tif cap(dst) >= len(params) {
\t\t\tcopiedParams = dst[:len(params)]
\t\t} else {
\t\t\tcopiedParams = make([]Param, len(params))
\t\t}
\t\tcopy(copiedParams, params)
\t}

\t// Validate read seqlock
\tseq2 := c.states[idx].seq.Load()
\tif seq1 != seq2 {
\t\t// Slot was modified while we were reading!"""
                content = content.replace(get_body_old, get_body_new)
                
                # 5. Fix Clear
                clear_old = """\t\t\toldParams := c.params[idx].Load()
\t\t\tif oldParams != nil {
\t\t\t\tputParamSlice(oldParams)
\t\t\t\tc.params[idx].Store(nil)
\t\t\t}"""
                clear_new = """\t\t\tc.params[idx].Store(nil)"""
                content = content.replace(clear_old, clear_new)
            
            # Now replace Get calls for tests/benchmarks
            # Be careful not to replace map.Get or otterCache.Get
            content = content.replace('lite.Get("GET", idStr)', 'lite.Get("GET", idStr, nil)')
            content = content.replace('lite.Get("GET", idStr);', 'lite.Get("GET", idStr, nil);')
            
            # We want to replace cache.Get and lite.Get
            # A regex is safer for this
            import re
            
            # Fix benchmarks/http_server/server.go
            if "server.go" in f:
                content = content.replace(
                    'if _, params, ok := lite.Get("GET", idStr); ok && len(params) > 0 {',
                    'var pbuf [1]liteLRU.Param\n\t\t\tif _, params, ok := lite.Get("GET", idStr, pbuf[:0]); ok && len(params) > 0 {'
                )
            
            # Fix benchmark and test files
            # For benchmark files, we want zero allocs, so we insert a buffer if possible.
            # But wait, in simple loops we can just pass `nil` if we don't care about allocs in misses, 
            # OR we can just pass a nil and it will alloc on hit.
            # Actually, in most benchmarks `params` is empty, so `len(params) > 0` is false, so it does NOT allocate!
            # Let's verify: `if len(params) > 0 { make... }`
            # Since the benchmarks (ablation, baseline, zipf) don't store params (they pass nil for params in Add),
            # `Get` will NOT allocate even if `dst` is nil!
            # So replacing `.Get(method, path)` with `.Get(method, path, nil)` is perfectly zero-alloc for them!
            
            content = re.sub(r'([a-zA-Z0-9_]+Cache)\.Get\(([^,]+),\s*([^)]+)\)', r'\1.Get(\2, \3, nil)', content)
            content = re.sub(r'lite\.Get\(([^,]+),\s*([^)]+)\)', r'lite.Get(\1, \2, nil)', content)
            content = re.sub(r'cache\.Get\(([^,]+),\s*([^)]+)\)', r'cache.Get(\1, \2, nil)', content)
            
            # Fix otterCache.Get which might have been accidentally matched if we called it otterCache
            # otterCache.Get only takes one argument: key
            # The regex `([^,]+),\s*([^)]+)` requires two arguments, so it won't match otterCache.Get(key) unless it had two args.
            
            with open(path, "w") as file:
                file.write(content)

print("done")
