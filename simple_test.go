package liteLRU
import (
	"testing"
	"fmt"
)
func TestSimple(t *testing.T) {
	c := NewLRUCache(128, 10)
	c.Add("GET", "/test", func(){}, nil)
	_, _, ok := c.Get("GET", "/test", nil)
	fmt.Println("Simple Hit:", ok)
}
