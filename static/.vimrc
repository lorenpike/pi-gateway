" Minimal vimrc

" ── Leader ──────────────────────────────────────────────────────────────
let mapleader = " "
let maplocalleader = " "

" ── Options ─────────────────────────────────────────────────────────────
set nocompatible
syntax on
filetype plugin indent on

set hlsearch&            " turn off search highlighting by default (like nvim config)
set nohlsearch
set number
set relativenumber
set mouse=a
set breakindent
set undofile
set ignorecase
set smartcase
set tabstop=4
set shiftwidth=4
set softtabstop=4
set expandtab
set updatetime=250
set scrolloff=8
set sidescrolloff=8
set nowrap
set signcolumn=yes
set completeopt=menuone,noselect
set hidden
set splitright
set splitbelow
set termguicolors       " close to the onedark feel when supported

" netrw (built-in file explorer) — same as your config
let g:netrw_preview = 1
let g:netrw_liststyle = 3
let g:netrw_browse_split = 4
let g:netrw_banner = 0   " hide the banner (vinegar-style)

" ── Vinegar-style '-' navigation ─────────────────────────────────────────
" '-' opens the current file's directory (netrw) in-place; inside netrw,
" '-' already lists the parent directory.
nnoremap <silent> - :Ex<CR>

" ── Keymaps ─────────────────────────────────────────────────────────────
nnoremap <Space> <Nop>
xnoremap <Space> <Nop>

" display-line movement (respects wrapped lines)
nnoremap <expr> k v:count == 0 ? 'gk' : 'k'
nnoremap <expr> j v:count == 0 ? 'gj' : 'j'

" highlight on yank (Neovim only — uses built-in vim.highlight)
if has('nvim')
  augroup YankHighlight
    autocmd!
    autocmd TextYankPost * silent! lua vim.highlight.on_yank()
  augroup END
endif

" terminal: easy window switch
tnoremap <C-w><C-w> <C-\><C-n><C-w><C-w>

" yanks to system clipboard
vnoremap <leader>y "+y
nnoremap <leader>yy "+yy
nnoremap <leader>Y  "+Y

" resize windows
nnoremap <silent> <C-Up>    :resize -2<CR>
nnoremap <silent> <C-Down>  :resize +2<CR>
nnoremap <silent> <C-Left>  :vertical resize -2<CR>
nnoremap <silent> <C-Right> :vertical resize +2<CR>

" reselect after indent
vnoremap < <gv
vnoremap > >gv

" centered paging
nnoremap <C-d> <C-d>zzzv
nnoremap <C-u> <C-u>zzzv

" move selected lines / current line
vnoremap <A-j> :m '>+1<CR>gv=gv
vnoremap <A-k> :m '<-2<CR>gv=gv
nnoremap <A-j> :m .+1<CR>==
nnoremap <A-k> :m .-2<CR>==

" quickfix: close after jump
augroup QfClose
  autocmd!
  autocmd FileType qf nnoremap <buffer> <silent> <CR> <CR>:cclose<CR>
augroup END

" ── Filetype conveniences (no plugins) ──────────────────────────────────
" Run the current file with <leader>x
augroup RunFile
  autocmd!
  autocmd FileType python nnoremap <buffer> <silent> <leader>x :!uv run %<CR>
  autocmd FileType lua    nnoremap <buffer> <silent> <leader>x :luafile %<CR>
augroup END
