package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/id"
	"fujipic/internal/model"
	"fujipic/internal/store"
	"fujipic/internal/upload"
)

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *model.User {
	user := s.requireWebUser(w, r)
	if user == nil {
		return nil
	}
	if !user.IsAdmin() {
		localizedError(w, "需要管理员权限", http.StatusForbidden)
		return nil
	}
	return user
}

type adminImageView struct {
	imageView
	OwnerEmail string
	BucketName string
}

type imageFilterOption struct {
	ID       string
	Name     string
	Selected bool
}

type adminImageFilterView struct {
	Search      string
	Users       []imageFilterOption
	Buckets     []imageFilterOption
	Filter      store.AdminImageFilter
	Active      bool
	Count       int
	Fingerprint string
	PreviousURL string
	NextURL     string
	ReturnURL   string
}

// Validate IDs against the complete option lists. Unknown/deleted IDs must not
// silently become an unrestricted query. Selecting every option means "all".
func imageFilterIDs(values []string, options []imageFilterOption) ([]string, error) {
	if len(values) > 1000 {
		return nil, store.ErrInvalidImageSelection
	}
	known := make(map[string]bool, len(options))
	for _, option := range options {
		known[option.ID] = true
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !known[value] {
			return nil, store.ErrInvalidImageSelection
		}
		seen[value] = true
	}
	if len(seen) == len(known) {
		return nil, nil
	}
	ids := make([]string, 0, len(seen))
	for value := range seen {
		ids = append(ids, value)
	}
	sort.Strings(ids)
	return ids, nil
}

func adminImageURL(f store.AdminImageFilter, page int) string {
	q := url.Values{}
	for _, value := range f.UserIDs {
		q.Add("user", value)
	}
	for _, value := range f.BucketIDs {
		q.Add("bucket", value)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/admin/images"
	}
	return "/admin/images?" + q.Encode()
}

func (s *Server) adminImageFilters(r *http.Request, values url.Values, lang string) (adminImageFilterView, error) {
	var view adminImageFilterView
	view.Search = values.Get("user_search")
	if len(view.Search) > 254 {
		return view, store.ErrInvalidImageSelection
	}
	var err error
	view.Filter.UserIDs, err = s.store.ValidateAdminImageUsers(r.Context(), values["user"])
	if err != nil {
		return view, err
	}
	users, err := s.store.SearchAdminImageUsers(r.Context(), view.Search, values["user"])
	if err != nil {
		return view, err
	}
	buckets, err := s.buckets.List(r.Context())
	if err != nil {
		return view, err
	}
	for _, user := range users {
		view.Users = append(view.Users, imageFilterOption{ID: user.ID, Name: user.Name})
	}
	for _, b := range buckets {
		name := b.Name
		if b.IsDefault {
			name = translate(lang, "服务器默认存储")
		}
		view.Buckets = append(view.Buckets, imageFilterOption{ID: b.ID, Name: name})
	}
	view.Filter.BucketIDs, err = imageFilterIDs(values["bucket"], view.Buckets)
	if err != nil {
		return view, err
	}
	// POST values describe the scope rendered on the reviewed page. Validate
	// them, but never broaden that scope when the option lists have changed.
	if r.Method == http.MethodPost {
		view.Filter.UserIDs = values["user"]
		view.Filter.BucketIDs = values["bucket"]
	}
	mark := func(options []imageFilterOption, ids []string) {
		selected := make(map[string]bool, len(ids))
		for _, id := range ids {
			selected[id] = true
		}
		for i := range options {
			options[i].Selected = selected[options[i].ID]
		}
	}
	mark(view.Users, view.Filter.UserIDs)
	mark(view.Buckets, view.Filter.BucketIDs)
	view.Active = view.Filter.Active()
	return view, nil
}

func (s *Server) handleAdminAuditClear(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.validCSRF(r) {
		localizedError(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	if err := s.store.ClearAudit(r.Context(), admin.ID, s.clientIP(r)); err != nil {
		s.log.Error("clear audit logs", "err", err)
		s.setFlash(w, "清理失败，请稍后重试。")
	} else {
		s.setFlash(w, "审计日志已清空。")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminInviteDelete(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.validCSRF(r) {
		localizedError(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	code := r.PathValue("code")
	switch err := s.store.DeleteInvite(r.Context(), code); {
	case err == nil:
		s.audit(r.Context(), admin.ID, "admin.invite_delete", code, s.clientIP(r), "")
		s.setFlash(w, "邀请码已删除，已注册用户不受影响。")
	case errors.Is(err, store.ErrNotFound):
		s.setFlash(w, "邀请码不存在或已删除。")
	default:
		s.log.Error("delete invite", "err", err)
		s.setFlash(w, "删除邀请码失败，请稍后重试。")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminImages(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 1_000_000 {
			localizedError(w, "页码不合法", http.StatusBadRequest)
			return
		}
	}
	const pageSize = 60
	filters, err := s.adminImageFilters(r, r.URL.Query(), responseLanguage(w))
	if err != nil {
		s.adminImageFilterError(w, err)
		return
	}
	filters.Count, filters.Fingerprint, err = s.store.AdminImageSummary(r.Context(), filters.Filter)
	if err != nil {
		localizedError(w, "加载图库失败", http.StatusInternalServerError)
		return
	}
	filters.ReturnURL = adminImageURL(filters.Filter, page)
	images, err := s.store.ListFilteredAdminImages(r.Context(), filters.Filter, pageSize+1, (page-1)*pageSize)
	if err != nil {
		s.log.Error("list admin images", "err", err)
		localizedError(w, "加载图库失败", http.StatusInternalServerError)
		return
	}
	data := pageData{Title: "全部图片"}
	if page > 1 {
		data.PreviousPage = page - 1
		filters.PreviousURL = adminImageURL(filters.Filter, page-1)
	}
	if len(images) > pageSize {
		data.NextPage = page + 1
		filters.NextURL = adminImageURL(filters.Filter, page+1)
		images = images[:pageSize]
	}
	for _, img := range images {
		bucketID := img.BucketID
		if bucketID == "" {
			bucketID = model.DefaultBucketID
		}
		bucketName := bucketID
		for _, option := range filters.Buckets {
			if option.ID == bucketID {
				bucketName = option.Name
				break
			}
		}
		data.AdminImages = append(data.AdminImages, adminImageView{
			imageView:  imageView{Image: img.Image, URL: s.uploads.URL(&img.Image), AlternateURLs: s.uploads.AlternateURLs(&img.Image), Name: displayName(img.OriginalName)},
			OwnerEmail: img.OwnerEmail,
			BucketName: bucketName,
		})
	}
	data.ImageFilter = filters
	s.render(w, r, "admin-images", data)
}

func (s *Server) adminImageFilterError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrInvalidImageSelection) {
		localizedError(w, "筛选条件无效，请重新选择用户和存储桶。", http.StatusBadRequest)
	} else {
		s.log.Error("load image filters", "err", err)
		localizedError(w, "加载图库失败", http.StatusInternalServerError)
	}
}

func (s *Server) handleAdminImagesDelete(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil || !s.validCSRF(r) {
		localizedError(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	filters, err := s.adminImageFilters(r, r.PostForm, responseLanguage(w))
	if err != nil {
		s.adminImageFilterError(w, err)
		return
	}
	mode := r.PostForm.Get("mode")
	if mode != "selected" && mode != "all" {
		localizedError(w, "请选择要删除的图片。", http.StatusBadRequest)
		return
	}
	// Deduplicate checked IDs without allowing an empty list to mean "all".
	seen := map[string]bool{}
	var ids []string
	for _, value := range r.PostForm["image"] {
		if value == "" || len(value) > 100 || len(ids) >= 1000 {
			localizedError(w, "请选择要删除的图片。", http.StatusBadRequest)
			return
		}
		if !seen[value] {
			ids = append(ids, value)
			seen[value] = true
		}
	}
	if mode == "all" {
		ids = nil
	}
	bases := append([]string{s.cfg.PublicBaseURL}, s.cfg.PublicBaseAliases...)
	count, err := s.store.QueueAdminImageDeletion(r.Context(), admin.ID, s.clientIP(r), filters.Filter, ids, mode == "all", r.PostForm.Get("fingerprint"), bases...)
	switch {
	case errors.Is(err, store.ErrImageSelectionTooLarge):
		localizedError(w, "筛选结果超过 1000 张，请缩小范围或分批勾选删除。", http.StatusBadRequest)
		return
	case errors.Is(err, store.ErrInvalidImageSelection):
		localizedError(w, "删除范围无效：全部图片视图仅支持勾选删除；筛选视图才可删除全部匹配结果。", http.StatusBadRequest)
		return
	case errors.Is(err, store.ErrAdminRequired):
		localizedError(w, "需要管理员权限", http.StatusForbidden)
		return
	case errors.Is(err, store.ErrImageSelectionChanged):
		s.setFlash(w, "筛选结果已变化，尚未删除任何图片。请核对最新结果后重试。")
	case err != nil:
		s.log.Error("bulk admin image deletion", "err", err)
		s.setFlash(w, "批量删除失败，未删除任何图片。请稍后重试。")
	default:
		s.setFlash(w, fmt.Sprintf(translate(responseLanguage(w), "已删除 %d 张图片，对象清理和 CDN 缓存刷新已排队。"), count))
	}
	http.Redirect(w, r, adminImageURL(filters.Filter, 1), http.StatusSeeOther)
}

func (s *Server) handleAdminImageDelete(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.validCSRF(r) {
		localizedError(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	err := s.uploads.DeleteAsAdmin(r.Context(), admin.ID, r.PathValue("id"), s.clientIP(r))
	switch {
	case err == nil:
		s.setFlash(w, "图片已删除，CDN 缓存清除已排队。")
	case errors.Is(err, store.ErrNotFound):
		s.setFlash(w, "图片不存在。")
	case errors.Is(err, upload.ErrAdminRequired):
		localizedError(w, "需要管理员权限", http.StatusForbidden)
		return
	default:
		s.log.Error("admin delete image", "err", err)
		s.setFlash(w, "删除失败，请稍后重试。")
	}
	redirect := "/admin/images"
	if target, err := url.Parse(r.FormValue("return_to")); err == nil && target.Path == "/admin/images" && target.Host == "" && target.Scheme == "" {
		redirect = target.String()
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.validCSRF(r) {
		localizedError(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	fail := func(message string) {
		s.setFlash(w, message)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
	if !s.limiter.allow("admin-create-user:"+admin.ID, 30, time.Hour) {
		fail("创建用户过于频繁，请稍后重试。")
		return
	}
	email := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, "@") || len(email) > 254 {
		fail("请输入有效的邮箱地址。")
		return
	}
	if !auth.ValidPasswordLength(password) {
		fail("密码长度需在 10 到 200 个字符之间。")
		return
	}
	if _, err := s.store.UserByEmail(r.Context(), email); err == nil {
		fail("该邮箱已注册。")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("check admin-created user", "err", err)
		fail("创建用户失败。")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.log.Error("hash admin-created password", "err", err)
		fail("创建用户失败。")
		return
	}
	now := time.Now().Unix()
	user := &model.User{ID: id.New("usr"), Email: email, PasswordHash: hash,
		Role: "user", Status: "active", QuotaBytes: s.cfg.DefaultQuotaBytes, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateUser(r.Context(), user); err != nil {
		if errors.Is(err, store.ErrConflict) {
			fail("该邮箱已注册。")
		} else {
			s.log.Error("admin create user", "err", err)
			fail("创建用户失败。")
		}
		return
	}
	s.audit(r.Context(), admin.ID, "admin.user_create", user.ID, s.clientIP(r), "")
	s.setFlash(w, "普通用户已创建，可使用设置的密码登录。配额可在用户列表调整。")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
