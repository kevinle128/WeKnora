package docparser

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHadongPageUnitsAssignPages(t *testing.T) {
	md := "<!-- Page 1 -->\n\nCÔNG TY CỔ PHẦN VTC\n\nQUYẾT ĐỊNH ban hành quy định\n\n<!-- Page 2 -->\n\nĐiều 5. Tiền vé máy bay được thanh toán theo chứng từ thực tế.\n"
	units := hadongPageUnits(md)
	require.Len(t, units, 3)
	assert.Equal(t, 1, units[0].Locator.Page)
	assert.Equal(t, 1, units[1].Locator.Page)
	assert.Equal(t, 2, units[2].Locator.Page)

	blocks := paddleOCRVLSourceBlocks(md, units, "pdf")
	require.Len(t, blocks, 3)
	last := blocks[2]
	assert.Equal(t, types.SourceLocatorPDF, last.Locator.Type)
	assert.Equal(t, 2, last.Locator.Page)
	assert.Equal(t, "exact", last.Locator.Mapping)
	assert.Contains(t, string([]rune(md)[last.Start:last.End]), "Điều 5")
}

func TestHadongPageUnitsWithoutMarkers(t *testing.T) {
	assert.Empty(t, hadongPageUnits("plain text\n\nno markers"))
	assert.Nil(t, paddleOCRVLSourceBlocks("x", hadongPageUnits("x"), "docx"))
}

func TestHadongPageUnitsAlignHTMLTables(t *testing.T) {
	md := "<!-- Page 3 -->\n\n<table><tr><td colspan=\"3\">TẠM ỨNG CÔNG TÁC PHÍ</td></tr><tr><td>Lập Giấy đề nghị</td></tr></table>\n\n" +
		"| STT | Tên tài liệu |\n|-----|------|\n| 1 | Biểu mẫu<br>Kế hoạch |\n"
	blocks := paddleOCRVLSourceBlocks(md, hadongPageUnits(md), "pdf")
	require.Len(t, blocks, 2)
	for _, b := range blocks {
		assert.Equal(t, 3, b.Locator.Page)
	}
}
