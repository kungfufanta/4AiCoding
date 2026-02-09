# Kỹ Năng: Dự Án Java Spring Boot + Docker với Tập Trung Vào Mẫu Kiến Trúc

## Giới Thiệu
Trong tài liệu này, chúng ta sẽ khám phá chi tiết về việc phát triển ứng dụng sử dụng Java Spring Boot trong môi trường Docker, tập trung vào các mẫu kiến trúc thiết kế.

## 1. Mẫu Kiến Trúc
### 1.1. Kiến Trúc Microservices
- **Định Nghĩa**: Phân chia ứng dụng thành các dịch vụ nhỏ độc lập.
- **Ưu điểm**: Khả năng mở rộng, bảo trì và triển khai độc lập.

### 1.2. Mẫu MVC (Model-View-Controller)
- **Định Nghĩa**: Tách biệt logic nghiệp vụ, giao diện người dùng và quản lý dữ liệu.
- **Cách Thực Hiện**: Sử dụng các annotation trong Spring để định nghĩa Controller, Service, và Repository.

## 2. Chi Tiết Về Dịch Vụ
### 2.1. Dịch Vụ Đăng Nhập
- **Tập Trung**: Xác thực người dùng.
- **Mits**: Thực hiện xác thực với JWT.
- **Đầu Vào**: Tên người dùng và mật khẩu.
- **Đầu Ra**: Token xác thực.

### 2.2. Dịch Vụ Đặt Hàng
- **Tập Trung**: Quản lý quy trình đặt hàng.
- **Mits**: Xử lý thông tin đặt hàng và tính toán giá trị.
- **Đầu Vào**: Chi tiết sản phẩm, thông tin người dùng.
- **Đầu Ra**: Xác nhận đặt hàng, chi tiết hóa đơn.

## 3. Cấu Trúc Dự Án
### 3.1. Cấu Trúc Thư Mục
```
project-root/
│
├── src/
│   ├── main/
│   │   ├── java/
│   │   │   └── com/example/project/
│   │   │       ├── controller/
│   │   │       ├── service/
│   │   │       ├── repository/
│   │   │       └── model/
│   │   └── resources/
│   └── test/
└── Dockerfile
```  

### 3.2. Dockerfile
```dockerfile
FROM openjdk:11-jre-slim
WORKDIR /app
COPY target/project.jar /app/project.jar
ENTRYPOINT [ "java", "-jar", "project.jar" ]
```  

## 4. Hướng Dẫn Triển Khai
### 4.1. Xây Dựng Hình Ảnh Docker
```bash
docker build -t <tên_hình_ảnh> .
```  

### 4.2. Chạy Ứng Dụng
```bash
docker run -p 8080:8080 <tên_hình_ảnh>
```  

## 5. Kết Luận
Tài liệu này đã cung cấp một cái nhìn tổng quan về cách phát triển ứng dụng Java Spring Boot chạy trên Docker với các mẫu kiến trúc thiết kế. Hãy tiếp tục nghiên cứu cho các trường hợp sử dụng cụ thể!