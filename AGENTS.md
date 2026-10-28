# Agents guide

This repository holds several small, independent projects. Each project
lives on its own branch. `master` holds only this guide; every project
branch starts from `master` and never merges back into it or into another
project.

Current projects:

- `audiopub`: EPUB to multi-voice audiobook converter
- `dashboard`: one-click redeploys (git + docker compose, or any commands), over SSH or local
- `trains`: Italian train delay tracker with notifications and a status page (Go)

## Working here

- Work only on the branch of the project you are asked about. Don't bring
  in files, dependencies or history from other projects' branches.
- To start a new project, branch from the head of `master`:

  ```sh
  git fetch origin master
  git checkout -b <name> origin/master
  git commit --allow-empty -m "Start <name> project"
  git push -u origin <name>
  ```

  Then add it to the list above with a commit on `master`.
- Each project keeps its own README, tooling and `.gitignore` at the root
  of its branch.
