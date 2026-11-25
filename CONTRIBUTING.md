# Contribuer à GMINI-Web-Proxy

Les contributions, retours d'expérience, rapports de bugs, suggestions d'amélioration et pull requests sont **chaleureusement bienvenus** !

---

## Comment contribuer

1. **Forker** le dépôt et créer une branche dédiée depuis `main` (`git checkout -b feat/ma-contribution`).
2. **Respecter l'architecture zéro-dépendance** : le proxy Go s'appuie uniquement sur la bibliothèque standard Go (`stdlib`), sans dépendance externe dans `go.mod`.
3. **Vérifier le code avant de soumettre** :
   ```bash
   gofmt -l .
   go vet ./...
   GOPROXY=off go test -race -count=1 ./...
   ```
   Si vous modifiez `ui.html`, vérifiez également la syntaxe JavaScript du bloc `<script>` avec `node --check`.
4. **Ne jamais polluer les fichiers utilisateur locaux** : les tests doivent rester isolés (`t.TempDir()`, `GEMINI_TOOLS_DB_FILE` temporaire) et ne jamais écrire dans `~/Library/Logs/gmini/`.
5. **Ouvrir une Pull Request** claire décrivant le problème résolu, le comportement attendu et les tests ajoutés.

---

## Signaler un bug ou partager un diagnostic

Avant d'ouvrir une issue :

- **Zéro secret, zéro log brut** : ne collez **jamais** `cookie.txt`, `.env`, `*.state.json`, clé API, jeton `at`, URL `/sorry` complète ou trace brute non anonymisée dans une issue publique.
- **Utilisez l'export anonymisé** :
  - Depuis la Web UI (`http://127.0.0.1:8081/_ui` → onglet **Diagnostic Proxy** → **Exporter**), ou
  - En ligne de commande via l'outil d'export anonymisé :
    ```bash
    python3 tools/export_gmini_diagnostics.py
    ```
  L'export anonymisé ne conserve que les horodatages, modèles, tailles, latences, statuts et identifiants techniques de requête (aucun prompt, aucune réponse, aucun cookie).

---

## Intégrer un nouveau modèle Gemini ou une nouvelle version d'Antigravity

- **Modèles Gemini** : le catalogue est lu dynamiquement depuis `otAQ7b` (`pkg/gemini/models.go`) et complété par les confirmations d'identité en direct (`inner[39]` / `inner[42]`). Si un nouveau modèle nécessite une table de repli ou un code de réflexion spécifique, vérifiez `pkg/gemini/models.go` et ajoutez un test unitaire dans `pkg/gemini/`.
- **Nouvelles versions d'Antigravity** : consultez [`README.md`](README.md) et [`SECURITY.md`](SECURITY.md), puis partagez les logs anonymisés et le comportement observé dans une issue ou PR.
