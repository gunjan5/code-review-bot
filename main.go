package main

import (
    "fmt"
    "io"

    "gopkg.in/src-d/go-git.v4"
    "gopkg.in/src-d/go-git.v4/utils/fs"
)

func main() {
    fs := fs.NewOS()
    path := ".git"

    repo, err := git.NewRepositoryFromFS(fs, path)
    if err != nil {
        panic(err)
    }

    iter, err := repo.Commits()
    if err != nil {
        panic(err)
    }
    defer iter.Close()

    for {
        commit, err := iter.Next()
        if err != nil {
            if err == io.EOF {
                break
            }
            panic(err)
        }

        fmt.Println(commit)
    }
}
