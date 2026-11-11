package server

import (
	"errors"
	"net/http"
	"net/mail"
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
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return nil
	}
	return user
}

type adminImageView struct {
	imageView
	OwnerEmail string
}

func (s *Server) handleAdminAuditClear(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	if err := s.store.ClearAudit(r.Context()); err != nil {
		s.log.Error("clear audit logs", "err", err)
		s.setFlash(w, "清理失败，请稍后重试。")
	} else {
		s.setFlash(w, "审计日志已清空。")
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
			http.Error(w, "页码不合法", http.StatusBadRequest)
			return
		}
	}
	const pageSize = 60
	images, err := s.store.ListAdminImages(r.Context(), pageSize+1, (page-1)*pageSize)
	if err != nil {
		s.log.Error("list admin images", "err", err)
		http.Error(w, "加载图库失败", http.StatusInternalServerError)
		return
	}
	data := pageData{Title: "全部图片"}
	if page > 1 {
		data.PreviousPage = page - 1
	}
	if len(images) > pageSize {
		data.NextPage = page + 1
		images = images[:pageSize]
	}
	for _, img := range images {
		data.AdminImages = append(data.AdminImages, adminImageView{
			imageView:  imageView{Image: img.Image, URL: s.uploads.URL(&img.Image), AlternateURLs: s.uploads.AlternateURLs(&img.Image), Name: displayName(img.OriginalName)},
			OwnerEmail: img.OwnerEmail,
		})
	}
	s.render(w, r, "admin-images", data)
}

func (s *Server) handleAdminImageDelete(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	err := s.uploads.DeleteAsAdmin(r.Context(), admin.ID, r.PathValue("id"), s.clientIP(r))
	switch {
	case err == nil:
		s.setFlash(w, "图片已删除，CDN 缓存清除已排队。")
	case errors.Is(err, store.ErrNotFound):
		s.setFlash(w, "图片不存在。")
	case errors.Is(err, upload.ErrAdminRequired):
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	default:
		s.log.Error("admin delete image", "err", err)
		s.setFlash(w, "删除失败，请稍后重试。")
	}
	http.Redirect(w, r, "/admin/images", http.StatusSeeOther)
}

func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
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
