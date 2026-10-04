package discord

import "testing"

func TestConvertTablesToBullets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no table",
			in:   "hello world\nno tables here",
			want: "hello world\nno tables here",
		},
		{
			name: "basic two-column",
			in:   "| Player | Score |\n|--------|-------|\n| Alice  | 150   |\n| Bob    | 120   |",
			want: "**Alice**\n• Score: 150\n\n**Bob**\n• Score: 120",
		},
		{
			name: "three-column skips heading column",
			in:   "| Name | Age | City |\n|:-----|----:|:----:|\n| Ada  |  30 | NYC  |",
			want: "**Ada**\n• Age: 30\n• City: NYC",
		},
		{
			name: "row-label column",
			in:   "|        | Score | Rank |\n|--------|-------|------|\n| Alice  | 150   | 1    |\n| Bob    | 120   | 2    |",
			want: "**Alice**\n• Score: 150\n• Rank: 1\n\n**Bob**\n• Score: 120\n• Rank: 2",
		},
		{
			name: "bare pipes without outer fences",
			in:   "head1 | head2\n--- | ---\na | b\nc | d",
			want: "**a**\n• head2: b\n\n**c**\n• head2: d",
		},
		{
			name: "table already in code block",
			in:   "```\n| a | b |\n| 1 | 2 |\n```",
			want: "```\n| a | b |\n| 1 | 2 |\n```",
		},
		{
			name: "surrounding prose kept",
			in:   "before\n| a | b |\n| - | - |\n| 1 | 2 |\nafter",
			want: "before\n**1**\n• b: 2\nafter",
		},
		{
			name: "pipe in regular text",
			in:   "use | for OR operations",
			want: "use | for OR operations",
		},
		{
			name: "pipe rows without separator not rewritten",
			in:   "text\n| x | y |\n| 1 | 2 |",
			want: "text\n| x | y |\n| 1 | 2 |",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertTablesToBullets(tt.in)
			if got != tt.want {
				t.Errorf("convertTablesToBullets():\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}
